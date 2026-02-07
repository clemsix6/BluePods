# BluePods Synchronization System

Ce document décrit le plan d'implémentation du système de synchronisation permettant à de nouveaux nœuds de rejoindre le réseau BluePods.

## Vue d'ensemble

```
┌─────────────────────────────────────────────────────────────────────┐
│                        SYNC WORKFLOW                                │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  Node A (bootstrap)              Node B (nouveau)                   │
│       │                                │                            │
│       │   [Snapshot périodique ~10s]   │                            │
│       │                                │                            │
│       │<──────── Connect (listener) ───│                            │
│       │════════ Gossip vertices ══════>│ [buffer en mémoire]        │
│       │                                │                            │
│       │<──────── RequestSnapshot ──────│                            │
│       │════════ Gossip continue ══════>│ [continue à buffer]        │
│       │                                │                            │
│       │─────── Snapshot response ─────>│                            │
│       │        (state only, no vertices)                            │
│       │                                │                            │
│       │                                │  1. Applique state         │
│       │                                │  2. Replay buffer depuis   │
│       │                                │     lastCommittedRound+1   │
│       │                                │  3. Sync complet ✓         │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

## Principe Clé : Pas de Redondance

- **Snapshot** = State committé uniquement (objets + lastCommittedRound)
- **Buffer** = Vertices reçus via gossip pendant le sync
- Les vertices ne sont jamais dupliqués entre snapshot et buffer

---

## Phase 1 : Écoute du Réseau (Listener Mode)

### Objectif

Permettre à n'importe quel nœud d'ouvrir une connexion QUIC et de recevoir tous les vertices gossipés sur le réseau, sans être validateur.

### Modifications Requises

#### 1.1 Nouveau mode "listener" dans la config

**Fichier** : `cmd/node/config.go`

```go
type Config struct {
    // ... existing fields ...

    // Listener indique que ce nœud écoute seulement le réseau
    // Il ne produit pas de vertices et n'est pas validateur
    Listener bool

    // BootstrapAddr est l'adresse du nœud bootstrap auquel se connecter
    // Requis si Listener=true ou Bootstrap=false
    BootstrapAddr string
}
```

**Nouveaux flags CLI** :
- `--listener` : Active le mode listener (écoute seule)
- `--bootstrap-addr` : Adresse QUIC du nœud bootstrap (ex: `localhost:9000`)

#### 1.2 Accepter les connexions non-validateur

**Fichier** : `internal/network/node.go`

Actuellement le réseau fonctionne en mesh entre validateurs connus. Il faut :

1. Accepter les connexions entrantes de nœuds inconnus
2. Leur envoyer le gossip des vertices comme aux autres
3. Ne pas attendre de vertices de leur part

```go
// Peer représente une connexion réseau
type Peer struct {
    // ... existing fields ...

    // IsListener indique si ce peer est en mode écoute seule
    // Les listeners reçoivent le gossip mais ne produisent pas
    IsListener bool
}
```

#### 1.3 Handshake initial

Quand un nouveau peer se connecte, il doit s'identifier :

```go
// HandshakeMessage est envoyé à la connexion pour s'identifier
type HandshakeMessage struct {
    // PublicKey du nœud (peut être nil pour listener anonyme)
    PublicKey []byte

    // IsListener indique si le nœud est en mode écoute seule
    IsListener bool

    // Version du protocole
    ProtocolVersion uint32
}
```

#### 1.4 Modification du node startup

**Fichier** : `cmd/node/node.go`

En mode listener :
- Ne pas démarrer le consensus (pas de production de vertices)
- Se connecter au bootstrap node
- Logger tous les vertices reçus

```go
func (n *Node) Start() error {
    if n.cfg.Listener {
        return n.startListenerMode()
    }
    return n.startValidatorMode()
}

func (n *Node) startListenerMode() error {
    // 1. Connecter au bootstrap
    // 2. Envoyer handshake (IsListener=true)
    // 3. Écouter et logger les vertices
}
```

### Critères de Validation Phase 1

```bash
# Terminal 1 : Bootstrap node
rm -rf data && go run ./cmd/node --bootstrap

# Terminal 2 : Listener node
rm -rf data-listener && go run ./cmd/node --listener --bootstrap-addr localhost:9000 --data ./data-listener

# Résultat attendu :
# - Le listener se connecte au bootstrap
# - Le listener affiche les mêmes logs de vertices que le bootstrap
# - Le listener ne produit PAS de vertices lui-même
```

**Logs attendus sur le listener** :
```
[LISTENER] connected to bootstrap localhost:9000
[LISTENER] received vertex round=0 txs=2 hash=abc123
[LISTENER] received vertex round=1 txs=0 hash=def456
[LISTENER] received vertex round=2 txs=0 hash=...
```

---

## Phase 2 : Système de Snapshots

### Objectif

Créer, compresser, transférer et appliquer des snapshots de l'état complet du réseau.

### Structure de la Snapshot

**Fichier** : `internal/sync/snapshot.go` (nouveau)

```go
// Snapshot représente l'état committé du réseau à un instant T
// Elle ne contient PAS les vertices - ceux-ci viennent du gossip live
type Snapshot struct {
    // Version du format de snapshot (pour migrations futures)
    Version uint32

    // LastCommittedRound est le dernier round dont les txs sont dans le state
    // Le node qui sync doit replay tous les vertices depuis round+1
    LastCommittedRound uint64

    // Objects contient tous les objets du state
    // Key: ObjectID (hash), Value: Object serialisé
    Objects map[Hash][]byte

    // Checksum pour vérifier l'intégrité
    Checksum [32]byte
}
```

### Snapshots Périodiques

Le bootstrap node crée une snapshot automatiquement :
- **MVP : toutes les 10 secondes** (pour faciliter les tests)
- Production : configurable (~60 secondes ou tous les N rounds)
- Stockée en mémoire (une seule, la plus récente)

```go
// SnapshotManager gère la création périodique de snapshots
type SnapshotManager struct {
    current   *Snapshot      // Snapshot la plus récente
    interval  time.Duration  // Intervalle entre snapshots
    mu        sync.RWMutex
}

// Latest retourne la snapshot la plus récente (pour les requêtes)
func (m *SnapshotManager) Latest() *Snapshot
```

### Modifications Requises

#### 2.1 Création de snapshot depuis Pebble

**Fichier** : `internal/sync/snapshot.go`

```go
// CreateSnapshot crée une snapshot de l'état committé actuel
func CreateSnapshot(storage *storage.Storage, dag *consensus.DAG) (*Snapshot, error) {
    // 1. Récupérer lastCommittedRound depuis le DAG
    // 2. Itérer sur tous les objets dans Pebble
    // 3. Calculer checksum
    // Note: PAS de vertices, ils viennent du gossip
}
```

#### 2.2 Compression de la snapshot

Utiliser zstd (déjà utilisé pour les vertices) :

```go
// Compress compresse la snapshot avec zstd
func (s *Snapshot) Compress() ([]byte, error)

// DecompressSnapshot décompresse et parse une snapshot
func DecompressSnapshot(data []byte) (*Snapshot, error)
```

#### 2.3 Protocole de transfert

**Fichier** : `internal/sync/protocol.go` (nouveau)

```go
// SnapshotRequest demande une snapshot au nœud distant
type SnapshotRequest struct {
    // RequestID pour corréler la réponse
    RequestID uint64
}

// SnapshotResponse contient la snapshot compressée
type SnapshotResponse struct {
    RequestID uint64

    // Data contient la snapshot compressée
    // Si trop gros, sera envoyé en chunks
    Data []byte

    // TotalSize pour afficher la progression
    TotalSize uint64

    // ChunkIndex si envoyé en plusieurs morceaux
    ChunkIndex uint32
    TotalChunks uint32
}
```

#### 2.4 Application de la snapshot

```go
// ApplySnapshot applique une snapshot reçue au storage local
func ApplySnapshot(storage *storage.Storage, snapshot *Snapshot) error {
    // 1. Vérifier le checksum
    // 2. Clear le storage existant (ou utiliser une nouvelle DB)
    // 3. Écrire tous les objets
    // 4. Sauvegarder lastCommittedRound pour le replay
}
```

#### 2.5 Handler côté serveur (bootstrap)

**Fichier** : `internal/sync/handler.go` (nouveau)

```go
// HandleSnapshotRequest traite une demande de snapshot
func HandleSnapshotRequest(req *SnapshotRequest, storage *storage.Storage, dag *consensus.DAG) (*SnapshotResponse, error)
```

### Critères de Validation Phase 2

```bash
# Terminal 1 : Bootstrap node (tourne quelques secondes pour générer du state)
rm -rf data && go run ./cmd/node --bootstrap
# Attendre ~10 secondes puis Ctrl+C

# Relancer le bootstrap
go run ./cmd/node --bootstrap

# Terminal 2 : Node qui demande une snapshot
rm -rf data-sync && go run ./cmd/node --sync-snapshot --bootstrap-addr localhost:9000 --data ./data-sync

# Résultat attendu :
# - Le node affiche la progression du téléchargement
# - La snapshot est appliquée
# - Le node affiche le state restauré (lastCommittedRound, nombre d'objets)
```

**Logs attendus** :
```
[SYNC] requesting snapshot from localhost:9000
[SYNC] downloading snapshot... 45% (2.3 MB / 5.1 MB)
[SYNC] downloading snapshot... 100% (5.1 MB / 5.1 MB)
[SYNC] decompressing snapshot...
[SYNC] verifying checksum...
[SYNC] applying snapshot: 42 objects, lastCommittedRound=156
[SYNC] snapshot applied successfully
```

---

## Phase 3 : Replay de Vertices

### Objectif

Rejouer les vertices depuis une snapshot pour reconstruire l'état exact, avec gestion des doublons et de l'ordre.

### Modifications Requises

#### 3.1 Vertex Buffer pendant le sync

**Fichier** : `internal/sync/buffer.go` (nouveau)

```go
// VertexBuffer stocke les vertices reçus via gossip pendant le sync
// Tous les vertices viennent du réseau, pas de la snapshot
type VertexBuffer struct {
    mu       sync.RWMutex
    vertices map[Hash]*Vertex      // Dedup par hash
    byRound  map[uint64][]*Vertex  // Index par round pour ordering
}

// Add ajoute un vertex au buffer (ignore si doublon)
func (b *VertexBuffer) Add(v *Vertex)

// GetOrderedSince retourne les vertices depuis un round, triés par round
func (b *VertexBuffer) GetOrderedSince(round uint64) []*Vertex

// Clear vide le buffer (après sync réussi)
func (b *VertexBuffer) Clear()
```

#### 3.2 Replay Engine

**Fichier** : `internal/sync/replay.go` (nouveau)

```go
// ReplayEngine rejoue les vertices pour reconstruire le state
type ReplayEngine struct {
    storage  *storage.Storage
    state    *state.State
    podvm    *podvm.Pool
    versions *consensus.VersionTracker
}

// Replay exécute les vertices dans l'ordre
func (r *ReplayEngine) Replay(vertices []*Vertex) error {
    // 1. Trier par round
    // 2. Pour chaque vertex :
    //    a. Valider la signature
    //    b. Pour chaque transaction :
    //       - Exécuter verify() puis execute()
    //       - Mettre à jour versions
    // 3. Vérifier la cohérence finale
}
```

#### 3.3 Vérification de cohérence

Après le replay, vérifier que le state est identique :

```go
// VerifyStateConsistency vérifie que le state après replay est cohérent
func VerifyStateConsistency(state *state.State, expectedRound uint64) error {
    // 1. Vérifier que tous les objets ont des versions cohérentes
    // 2. Vérifier le lastCommittedRound
    // 3. Optionnel: calculer un hash global du state pour comparaison
}
```

### Critères de Validation Phase 3

```bash
# Terminal 1 : Bootstrap node
rm -rf data && go run ./cmd/node --bootstrap
# Laisser tourner ~30 secondes pour générer des vertices

# Terminal 2 : Node qui sync avec replay
rm -rf data-sync && go run ./cmd/node --sync-full --bootstrap-addr localhost:9000 --data ./data-sync

# Résultat attendu :
# - Download de la snapshot
# - Replay des vertices buffered depuis le gossip
# - State final identique au bootstrap
```

**Logs attendus** :
```
[SYNC] snapshot applied: lastCommittedRound=156, objects=42
[SYNC] buffer contains 24 vertices from gossip
[SYNC] filtering vertices: keeping rounds >= 157
[SYNC] replaying 24 vertices from round 157 to 180...
[SYNC] replaying round 157: 1 vertex, 0 txs
[SYNC] replaying round 158: 1 vertex, 0 txs
...
[SYNC] replay complete: 24 vertices processed
[SYNC] state verification: OK
[SYNC] current round: 180
```

**Test de cohérence** :
```bash
# Comparer les states (ajouter une commande debug)
go run ./cmd/node --dump-state --data ./data > state-bootstrap.txt
go run ./cmd/node --dump-state --data ./data-sync > state-sync.txt
diff state-bootstrap.txt state-sync.txt
# Doit être vide (pas de différence)
```

---

## Phase 4 : Sync Complet

### Objectif

Assembler les 3 phases précédentes en un système de synchronisation complet et automatique.

### Workflow Complet

```go
// SyncManager orchestre le processus de synchronisation complet
type SyncManager struct {
    config       *Config
    network      *network.Node
    storage      *storage.Storage
    buffer       *VertexBuffer
    replayEngine *ReplayEngine
}

func (s *SyncManager) Sync(bootstrapAddr string) error {
    // Phase 1: Connecter et écouter (buffer les vertices en arrière-plan)
    if err := s.connectAndListen(bootstrapAddr); err != nil {
        return err
    }

    // Phase 2: Demander et appliquer la snapshot (state only)
    snapshot, err := s.requestSnapshot()
    if err != nil {
        return err
    }
    if err := s.applySnapshot(snapshot); err != nil {
        return err
    }

    // Phase 3: Replay des vertices buffered depuis le gossip
    // Filtrer: garder seulement les vertices après lastCommittedRound
    vertices := s.buffer.GetOrderedSince(snapshot.LastCommittedRound + 1)
    if err := s.replayEngine.Replay(vertices); err != nil {
        return err
    }

    // Sync terminé, clear le buffer et passer en mode normal
    s.buffer.Clear()
    return s.transitionToNormalMode()
}
```

### Modes de Fonctionnement

Après la phase 4, le nœud supporte 3 modes :

1. **Bootstrap** (`--bootstrap`)
   - Premier nœud du réseau
   - Crée le genesis
   - Produit des vertices

2. **Validator** (défaut, avec `--bootstrap-addr`)
   - Se sync au démarrage
   - Puis produit des vertices
   - Participe au consensus

3. **Listener** (`--listener --bootstrap-addr`)
   - Se sync au démarrage
   - Écoute seulement
   - Ne produit pas de vertices

### Critères de Validation Phase 4

#### Test 1 : Sync basique

```bash
# Terminal 1 : Bootstrap
rm -rf data && go run ./cmd/node --bootstrap

# Terminal 2 : Nouveau validateur (sync + rejoint le consensus)
rm -rf data2 && go run ./cmd/node --bootstrap-addr localhost:9000 --data ./data2 --quic :9001 --http :8081

# Résultat attendu :
# - Node 2 se sync
# - Node 2 commence à produire des vertices
# - Les deux nodes voient les vertices de l'autre
```

#### Test 2 : Sync pendant activité

```bash
# Terminal 1 : Bootstrap avec transactions
rm -rf data && go run ./cmd/node --bootstrap
# Envoyer quelques transactions via HTTP API pendant ce temps

# Terminal 2 : Rejoindre pendant l'activité
rm -rf data2 && go run ./cmd/node --bootstrap-addr localhost:9000 --data ./data2 --quic :9001 --http :8081

# Résultat attendu :
# - Sync réussit malgré l'activité continue
# - Pas de perte de transactions
# - States identiques
```

#### Test 3 : Listener mode continu

```bash
# Terminal 1 : Bootstrap
rm -rf data && go run ./cmd/node --bootstrap

# Terminal 2 : Listener permanent
rm -rf data-listener && go run ./cmd/node --listener --bootstrap-addr localhost:9000 --data ./data-listener

# Laisser tourner ~30 secondes
# Vérifier que le listener a tous les vertices
```

---

## Structure des Fichiers à Créer

```
internal/
└── sync/
    ├── sync.go         # SyncManager principal
    ├── snapshot.go     # Création/application de snapshots
    ├── buffer.go       # VertexBuffer pour stockage temporaire
    ├── replay.go       # ReplayEngine pour rejouer les vertices
    ├── protocol.go     # Messages de synchronisation
    └── handler.go      # Handlers côté serveur
```

## Dépendances entre Phases

```
Phase 1 (Listener) ──┐
                     ├──> Phase 4 (Sync Complet)
Phase 2 (Snapshot) ──┤
                     │
Phase 3 (Replay) ────┘
```

Les phases 1, 2 et 3 peuvent être développées en parallèle car elles sont relativement indépendantes. La phase 4 les assemble.

---

## Estimation de Complexité

| Phase | Fichiers | Complexité | Description |
|-------|----------|------------|-------------|
| 1 | 3-4 | Moyenne | Modifications réseau + nouveau mode |
| 2 | 2-3 | Moyenne | Snapshot creation/compression/transfer |
| 3 | 2 | Moyenne | Buffer + replay engine |
| 4 | 1-2 | Faible | Orchestration des autres phases |

## Notes d'Implémentation

### Gestion des erreurs pendant le sync

Si le sync échoue à mi-chemin :
1. Clear le state partiel
2. Retry depuis le début
3. Après N échecs, abandonner avec erreur claire

### Progression et logs

Afficher une progression claire pour l'utilisateur :
- Pourcentage de download
- Nombre de vertices reçus/rejoués
- État final

### Futur : Optimisations

Ces optimisations ne sont PAS dans le scope initial mais à garder en tête :
- Chunked snapshots pour gros states
- Merkle proofs pour sync partiel
- Parallel replay de vertices indépendants
- Checkpoint périodiques pour sync plus rapide
