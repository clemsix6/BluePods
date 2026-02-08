# Epochs : Connexion et Deconnexion des Validators

Document de design pour la gestion des validators rejoignant ou quittant le reseau.

---

## Object Tracker

### Probleme

Quand un validator rejoint le reseau, il doit savoir quels objets il doit stocker (via Rendezvous Hashing). Pour calculer le Rendezvous, il faut connaitre l'ID et le facteur de replication de chaque objet du reseau.

### Solution

Chaque validator maintient un **object tracker** qui stocke `{ID, version, replication}` pour **tous** les objets du reseau. Ce tracker est persiste sur SSD via Pebble (LSM-tree) et inclus dans les snapshots.

Format par objet : 32 bytes (ID) + 8 bytes (version) + 2 bytes (replication) = **42 bytes**.

| Objets       | Taille SSD | Taille RAM cache |
|-------------|-----------|-----------------|
| 1 million   | ~42 MB    | negligeable     |
| 100 millions| ~4.2 GB   | negligeable     |
| 1 milliard  | ~42 GB    | 128-256 MB hot  |

Le tracker remplace le `versionTracker` actuel (en memoire, `map[Hash]uint64`) par une persistence Pebble. Les objets frequemment accedes restent dans le block cache de Pebble (objets hot). Le scan complet ne se fait qu'a l'epoch transition.

### Mise a jour du tracker

- **Creation d'objet** : le commit ajoute l'entree `{ID, version=1, replication}` au tracker.
- **Modification d'objet** : le commit incremente la version dans le tracker.
- **Suppression d'objet** : le commit supprime l'entree du tracker.

Tous les validators maintiennent le meme tracker car tous observent les memes transactions committees dans le DAG. Le tracker est deterministe.

---

## Connexion d'un nouveau validator

### Etape 1 : Sync initiale

Le nouveau validator V contacte un noeud existant P et telecharge une snapshot contenant :
- Les objets stockes par P (singletons + objets dont P est holder)
- L'object tracker complet (tous les IDs + versions + replication)
- Le ValidatorSet courant
- Les vertices recents du DAG

### Etape 2 : Calcul des responsabilites

V scanne l'object tracker et calcule le Rendezvous Hashing pour chaque objet :
- `score = blake3(objectID || validatorPubkey)` pour chaque validator
- Si V est dans le top-N (N = replication de l'objet) → V est holder

Les singletons (replication=0) sont deja presents via la snapshot (tout le monde les a).

### Etape 3 : Telechargement des objets manquants

Pour chaque objet dont V est holder mais qu'il n'a pas recu dans la snapshot de P :
- V contacte les holders existants via QUIC pour telecharger l'objet complet
- Le telechargement se fait en background

### Etape 4 : Fallback lazy

Si une transaction arrive touchant un objet dont V est holder mais qu'il n'a pas encore telecharge :
- V fetch l'objet a la demande depuis les holders existants
- V ne vote que sur les objets qu'il possede (pas de vote negatif artificiel)

### Taille du telechargement

Avec 1M objets, replication=10, et 5000 validators → chaque validator hold ~2000 objets. A 4 KB max par objet → ~8 MB. Meme avec 100M objets → 800 MB. Negligeable.

---

## Deconnexion d'un validator

### Intra-epoch : impact immediat

Quand un validator tombe en cours d'epoch, **rien de special ne se passe**. L'agregateur contacte les holders, certains repondent, certains ne repondent pas. L'agregateur envoie la transaction **des que le quorum est atteint (67%)** sans attendre les retardataires.

Avec replication=10 et quorum=7 :
- 1 holder down → 9 restants → quorum atteignable
- 3 holders down → 7 restants → quorum juste atteint
- 4+ holders down → quorum impossible → les objets concernes sont bloques jusqu'a l'epoch suivante

Aucune coordination necessaire. L'absence d'un holder est geree naturellement par le timeout de l'agregateur.

### Pourquoi les bitmaps ne servent pas a la detection

L'agregateur envoie des que 67% des holders ont repondu. Le signer bitmap dans le quorum proof reflète donc toujours ~67% des holders, que les 33% restants soient down ou juste plus lents. On ne peut pas distinguer "inactif" de "legerement en retard" depuis le bitmap.

### Detection d'inactivite : production de vertices

Chaque validator produit des vertices dans le DAG a chaque round. Un validator down **arrete de produire des vertices**. C'est observable par tous, deterministe, et deja dans le DAG.

Aucun heartbeat, aucun protocole supplementaire. Le silence dans le DAG est le signal.

### Epoch boundary : exclusion deterministe

A la fin de l'epoch, chaque validator calcule independamment les memes statistiques de participation :

```
Pour chaque validator V:
  attendu  = nombre de rounds dans l'epoch
  produit  = nombre de vertices de V dans le DAG committe
  participation = produit / attendu
```

Ce calcul est purement deterministe : memes vertices committes → meme resultat pour tout le monde.

- `participation < 67%` → penalite progressive sur le stake (quand le staking existera)
- `participation < 33%` sur plusieurs epochs consecutives → **exclusion** du set de l'epoch suivante

L'exclusion ne necessite aucun vote ni transaction speciale. Tous les validators arrivent a la meme conclusion independamment depuis le DAG.

### Redistribution des objets

Quand un validator est exclu a l'epoch boundary :
1. Le Rendezvous est recalcule avec le nouveau ValidatorSet
2. Pour chaque objet dont l'exclu etait holder, le (N+1)eme dans le classement Rendezvous devient le nouveau holder
3. Les N-1 autres holders restent inchanges (propriete du Rendezvous Hashing : stabilite lors des changements)
4. Le nouveau holder telecharge l'objet depuis les holders restants (meme mecanisme que la connexion)

---

## Depart volontaire

Un validator qui souhaite quitter proprement peut annoncer son depart via une transaction au system pod. Cette annonce permet de :
- Declencher la redistribution de ses objets **avant** l'epoch boundary
- Eviter d'etre penalise pour inactivite
- Reduire la fenetre ou ses objets ont un holder en moins

Le depart prend effet a l'epoch suivante. Le validator continue de participer jusqu'a la fin de l'epoch courante.

---

## Resume

| Evenement | Mecanisme | Coordination |
|-----------|-----------|-------------|
| Nouveau validator | Snapshot + tracker scan + download QUIC | Aucune |
| Holder down (intra-epoch) | Timeout agregateur, quorum avec les restants | Aucune |
| Detection inactivite | Production de vertices dans le DAG | Aucune (deterministe) |
| Exclusion (epoch boundary) | Calcul participation = vertices / rounds | Aucune (deterministe) |
| Redistribution objets | Rendezvous recalcule, download depuis holders | Aucune |
| Depart volontaire | Transaction au system pod | Transaction unique |
