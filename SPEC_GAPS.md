# Ecarts avec bluepods_v2.md

Liste des simplifications et ecarts entre l'implementation actuelle et la spec `bluepods_v2.md`.

---

## Implemente et fonctionnel

Les elements suivants de la spec sont correctement implementes :

- **Singletons vs objets standard** : Les singletons (replication=0) sont exclus du body ATX, resolus depuis le state local via `resolveMutableObjects()`. Les objets standard sont collectes aupres des holders.
- **Attestation BLS / collecte de quorum** : L'agregateur collecte les attestations des holders via QUIC, le top-1 envoie l'objet complet, les autres envoient hash+signature BLS. Quorum 67% avec fail-fast sur votes negatifs. Preuves incluses dans les vertices (signature BLS agregee + bitmap).
- **Rendezvous Hashing** : Les holders sont determines par Rendezvous Hashing (`blake3(objectID || validatorPubkey)`, tri decroissant, top-N). Implemente dans `aggregation/rendezvous.go`.
- **Sharding d'execution** : Seuls les holders des MutableObjects executent. Configure via `dag.SetIsHolder()` et `state.SetIsHolder()` dans `cmd/node/node.go`. Exception : `creates_objects=true` → tous les validators executent.
- **Sharding de stockage** : Les objets crees ne sont stockes que par les holders (via `isHolder` dans `state.applyCreatedObjects()`).
- **Version tracking** : Le `versionTracker` dans le consensus suit les versions de tous les objets depuis le DAG. Detection de conflits sans execution. `ensureMutableVersions()` garantit la coherence state/consensus.
- **Consensus DAG (Mysticeti)** : Commit rule 2-round avec quorum 67%. Vertices gossiped avec fanout 40. Verification des preuves BLS au commit.
- **IDs deterministes** : `blake3(txHash || index_u32_LE)` pour les objets crees.
- **ReadObjects** : Le client et le consensus supportent `ReadObjects` (version verifiee, pas incrementee) et `MutableObjects` (version verifiee ET incrementee). `checkVersions()` verifie les deux listes.

---

## Ecarts restants

### 1. Pas de limites de transaction enforçees

**Spec** :
- 8 objets standards max par tx
- 32 singletons max par tx
- 16 objets crees max par tx
- 48 KB max par tx
- 4 KB max par objet

**Actuel** : Aucune de ces limites n'est verifiee cote noeud ou client.

**Impact** : Un client peut soumettre des transactions arbitrairement grandes.

**Fix** : Ajouter la validation dans `handleSubmitTx` et/ou dans le consensus avant execution.

---

### 2. Pas d'epochs / gestion des validators

**Spec** : Le reseau fonctionne par epochs. Au debut de chaque epoch, la liste des validators est figee. Detection des inactifs par observation des votes. Exclusion et penalites en fin d'epoch.

**Actuel** : Pas d'epochs. Les validators sont ajoutes dynamiquement et ne sont jamais retires. Pas de detection d'inactivite, pas de penalites, pas de slashing.

**Impact** : Pas de rotation des validators. Un validator inactif reste dans le set indefiniment.

---

### 3. Pas de systeme de fees

**Spec** : Les frais de transaction existent (creation de singletons plus chere, enregistrement de domaines dissuasif).

**Actuel** : Aucun systeme de fees. Les transactions sont gratuites. Gas limit hardcode a 10M.

---

### 4. Pas de Domain Registry

**Spec** : Systeme de noms de domaine (DomainRegistry singleton) permettant d'associer des noms lisibles a des adresses d'objets.

**Actuel** : Non implemente.

---

### 5. Pas de fraud proofs

**Spec** : "Le mecanisme exact de fraud proof si un holder execute mal une transaction et produit un mauvais etat n'est pas encore defini." (probleme ouvert dans la spec aussi)

**Actuel** : Aucune verification que les holders produisent le meme resultat d'execution.

---

### 6. Pas de challenge de stockage

**Spec** : "Les details du systeme de challenge et proof pour le stockage restent a affiner." (probleme ouvert dans la spec aussi)

**Actuel** : Aucun mecanisme pour verifier qu'un holder detient reellement les objets.

---

### 7. Pas de backpressure sur la mempool

**Spec** : Non couvert par la spec.

**Actuel** : `pendingTxs` dans le DAG est un slice non borne. Si le debit de soumission depasse le debit de traitement du reseau, la memoire explose sans aucune limite.

**Impact** : Vecteur de DOS. Un noeud sous charge excessive finit en OOM.

**Fix** : Borner `pendingTxs` avec un cap. Quand la mempool est pleine, rejeter les nouvelles tx (HTTP 503) pour que le client resoumettre a un autre validator. A terme avec les fees, eviction par fee la plus basse (priority queue).

---

### 8. Pas de pruning / retention limitee de l'historique

**Spec** : Non couvert par la spec.

**Actuel** : Les vertices committes et executes restent en memoire/stockage indefiniment. Pas de purge des anciennes donnees.

**Impact** : Le stockage croit indefiniment. Stocker l'integralite de l'historique depuis le genesis est inutile et couteux (Solana: ~400 TB de ledger, +80 TB/an).

**Decision** : Les validators ne stockent que le state courant (objets + versions) et les N derniers rounds du DAG (suffisant pour la commit rule et les retards reseau). L'historique complet n'est pas necessaire pour la securite du consensus. Ce choix est aligne avec l'industrie : Solana prune en FIFO (`--limit-ledger-size`), NEAR garbage-collect apres 5 epochs (~2.5 jours), Sui supporte le pruning agressif, Ethereum implemente EIP-4444 (history expiry).

**Audit et tracabilite** : Un systeme de logs emis par les pods (a la Solana Program Logs) permettra aux developpeurs de logger ce qui est pertinent pour leur application. Ces logs seront indexes off-chain par des services externes (explorateurs, indexeurs), pas par les validators. Les archive nodes seront optionnels pour ceux qui veulent l'historique complet.

**Fix** : Implementer le pruning des vertices committes au-dela de N rounds. Definir le mecanisme de snapshot signe (hash du state signe par un quorum de validators) pour le sync de nouveaux noeuds.

---

## Priorite suggeree

1. **Point 1** (limites tx) - Securite basique, empecher les abus
2. **Point 2** (epochs) - Gros chantier, necessaire pour un reseau ouvert
3. **Point 3** (fees) - Necessaire avant mainnet
4. **Point 7** (backpressure mempool) - Securite basique, empecher les OOM
5. **Point 8** (pruning) - Necessaire pour la viabilite long terme
6. **Points 4, 5, 6** - Futures iterations
