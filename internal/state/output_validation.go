package state

import (
	"bytes"
	"fmt"

	"BluePods/internal/types"
)

const (
	// parentKindKeyRoot marks a created object's parent bytes as an Ed25519
	// public key: the object roots directly at a key.
	parentKindKeyRoot byte = 0

	// parentKindObject marks a created object's parent bytes as another object's
	// 32-byte ID: the object hangs under a parent object.
	parentKindObject byte = 1
)

// validateOutput enforces every pod-output invariant before any mutation is
// applied: creation and domain limits, domain-name well-formedness, the
// creation-permission rule, parent immutability on updated objects, and the
// pod-output deletion carve-out. The first failing check reverts the whole
// output (fees stay kept by the caller).
func (s *State) validateOutput(output *types.PodExecuteOutput, tx *types.Transaction, txHash Hash, inputs []*types.Object) error {
	if err := validateCreationLimits(output, tx); err != nil {
		return err
	}

	if err := s.validateDomainRegistrations(output); err != nil {
		return err
	}

	if err := s.validateCreatedParents(output, tx, txHash); err != nil {
		return err
	}

	if err := validateParentImmutability(output, inputs); err != nil {
		return err
	}

	return validateOutputDeletions(output, tx, inputs)
}

// validateCreationLimits rejects an output that creates more objects or
// registers more domains than the transaction header permits.
func validateCreationLimits(output *types.PodExecuteOutput, tx *types.Transaction) error {
	if output.CreatedObjectsLength() > tx.CreatedObjectsReplicationLength() {
		return fmt.Errorf("created %d objects, max allowed %d", output.CreatedObjectsLength(), tx.CreatedObjectsReplicationLength())
	}

	if output.RegisteredDomainsLength() > int(tx.MaxCreateDomains()) {
		return fmt.Errorf("registered %d domains, max allowed %d", output.RegisteredDomainsLength(), tx.MaxCreateDomains())
	}

	return nil
}

// validateDomainRegistrations rejects empty, over-long, duplicate, or colliding
// domain names in the output.
func (s *State) validateDomainRegistrations(output *types.PodExecuteOutput) error {
	seen := make(map[string]bool, output.RegisteredDomainsLength())
	var dom types.RegisteredDomain

	for i := 0; i < output.RegisteredDomainsLength(); i++ {
		if !output.RegisteredDomains(&dom, i) {
			continue
		}

		if err := s.validateDomainName(string(dom.Name()), seen); err != nil {
			return err
		}
	}

	return nil
}

// validateDomainName checks one domain name against the length bounds, the
// names already seen in this output, and the registered set.
func (s *State) validateDomainName(name string, seen map[string]bool) error {
	if len(name) == 0 {
		return fmt.Errorf("empty domain name")
	}

	if len(name) > 253 {
		return fmt.Errorf("domain name too long: %d bytes (max 253)", len(name))
	}

	if seen[name] {
		return fmt.Errorf("duplicate domain name in output: %q", name)
	}
	seen[name] = true

	if s.domains.exists(name) {
		return fmt.Errorf("domain collision: %q already registered", name)
	}

	return nil
}

// validateCreatedParents enforces the creation-permission rule: every created
// object's declared parent must be the sender's own key, an object the sender
// controls, an object this transaction reaches through a domain reference, or an
// object created earlier in this same output. Same-output parents are staged in
// output order so a nested creation (B under a just-created A) is authorized
// before the tracker has learned A.
func (s *State) validateCreatedParents(output *types.PodExecuteOutput, tx *types.Transaction, txHash Hash) error {
	sender := extractSender(tx)
	domainParents := s.domainReferencedParents(tx)
	staged := make(map[Hash]bool, output.CreatedObjectsLength())

	var obj types.Object
	for i := 0; i < output.CreatedObjectsLength(); i++ {
		if !output.CreatedObjects(&obj, i) {
			continue
		}

		if !s.createdParentAllowed(obj.ParentKind(), objectOwner(&obj), sender, staged, domainParents, tx) {
			return fmt.Errorf("created object %d declares an unauthorized parent", i)
		}

		staged[computeObjectID(txHash, uint32(i))] = true
	}

	return nil
}

// createdParentAllowed reports whether a created object's declared parent
// satisfies the creation-permission rule. A KeyRoot parent must be the sender's
// own key. An object-parent is allowed when it was created earlier in this
// output, is reached through a domain reference, or is controlled by the sender
// per the consensus cascade walk.
func (s *State) createdParentAllowed(kind byte, parent, sender Hash, staged, domainParents map[Hash]bool, tx *types.Transaction) bool {
	if kind == parentKindKeyRoot {
		return parent == sender
	}

	if staged[parent] || domainParents[parent] {
		return true
	}

	return s.parentValidator != nil && s.parentValidator(kind, parent, sender, tx)
}

// domainReferencedParents resolves the object IDs this transaction reaches
// through a mutable domain reference, mirroring the shared-access ownership
// exemption the commit path already grants domain refs. A created object may
// hang under any of these shared, domain-named objects.
func (s *State) domainReferencedParents(tx *types.Transaction) map[Hash]bool {
	parents := make(map[Hash]bool)

	var ref types.ObjectRef
	for i := 0; i < tx.MutableRefsLength(); i++ {
		if !tx.MutableRefs(&ref, i) {
			continue
		}

		name := ref.Domain()
		if len(name) == 0 {
			continue
		}

		if id, ok := s.domains.get(string(name)); ok {
			parents[id] = true
		}
	}

	return parents
}

// validateParentImmutability rejects any updated object whose owner bytes or
// parent kind differ from its attested input copy. Pods may only change an
// object's content; reparenting and transfers go through declared operations at
// commit, never through a pod update. An updated object with no matching input
// is rejected outright, since a pod can only update an object it was given.
func validateParentImmutability(output *types.PodExecuteOutput, inputs []*types.Object) error {
	byID := inputsByID(inputs)

	var obj types.Object
	for i := 0; i < output.UpdatedObjectsLength(); i++ {
		if !output.UpdatedObjects(&obj, i) {
			continue
		}

		input, ok := byID[objectIDOf(&obj)]
		if !ok {
			return fmt.Errorf("updated object %d has no attested input copy", i)
		}

		if obj.ParentKind() != input.ParentKind() || !bytes.Equal(obj.OwnerBytes(), input.OwnerBytes()) {
			return fmt.Errorf("updated object %d changed its parent", i)
		}
	}

	return nil
}

// validateOutputDeletions rejects a pod output that deletes objects unless the
// transaction is globally executed, so the deletion is applied uniformly on
// every node. Global execution holds when the transaction creates objects or
// when every mutable reference is a singleton (the merge carve-out); a sharded
// deletion in any other transaction is rejected.
func validateOutputDeletions(output *types.PodExecuteOutput, tx *types.Transaction, inputs []*types.Object) error {
	if output.DeletedObjectsLength() == 0 {
		return nil
	}

	if tx.CreatedObjectsReplicationLength() > 0 || allMutableRefsSingletons(tx, inputs) {
		return nil
	}

	return fmt.Errorf("pod output deletes objects in a transaction that is not globally executed")
}

// allMutableRefsSingletons reports whether every mutable reference resolves to a
// singleton, which is the network-uniform proxy for "every node executes this
// transaction": all validators hold every singleton. A reference not carried in
// the inputs defaults to a singleton, matching the commit-path holder check; a
// domain reference, whose target replication is not proven here, is treated as
// non-singleton.
func allMutableRefsSingletons(tx *types.Transaction, inputs []*types.Object) bool {
	replication := replicationByID(inputs)

	var ref types.ObjectRef
	for i := 0; i < tx.MutableRefsLength(); i++ {
		if !tx.MutableRefs(&ref, i) {
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			return false
		}

		var id Hash
		copy(id[:], idBytes)

		if replication[id] > 0 {
			return false
		}
	}

	return true
}

// inputsByID indexes the attested input objects by their 32-byte ID.
func inputsByID(inputs []*types.Object) map[Hash]*types.Object {
	byID := make(map[Hash]*types.Object, len(inputs))

	for _, obj := range inputs {
		if obj == nil {
			continue
		}

		byID[objectIDOf(obj)] = obj
	}

	return byID
}

// replicationByID indexes each attested input object's replication by its ID.
func replicationByID(inputs []*types.Object) map[Hash]uint16 {
	replication := make(map[Hash]uint16, len(inputs))

	for _, obj := range inputs {
		if obj == nil {
			continue
		}

		replication[objectIDOf(obj)] = obj.Replication()
	}

	return replication
}

// objectIDOf copies an object's 32-byte ID into a Hash, or the zero hash when it
// is absent or malformed.
func objectIDOf(obj *types.Object) Hash {
	var id Hash
	if b := obj.IdBytes(); len(b) == 32 {
		copy(id[:], b)
	}

	return id
}

// objectOwner copies an object's 32-byte owner (its parent bytes) into a Hash,
// or the zero hash when it is absent or malformed.
func objectOwner(obj *types.Object) Hash {
	var owner Hash
	if b := obj.OwnerBytes(); len(b) == 32 {
		copy(owner[:], b)
	}

	return owner
}
