# Domain Registry

Systeme de noms de domaine pour associer des identifiants lisibles a des ObjectID.
Ex: `system.validators` au lieu de `0x7a3f8b2c...`

---

## Architecture

Le registre est stocke dans **Pebble** sur chaque node, au niveau protocole.
Ce n'est PAS un singleton/objet. C'est un index local maintenu par le protocole.

Pourquoi pas un singleton :
- Un singleton de 8 MB relu et reecrit a chaque enregistrement explose le gas
- Le cout croit avec le nombre de domaines enregistres
- Le registre est de l'infra, pas de la logique metier

Pourquoi Pebble fonctionne :
- Tous les validators traitent les memes transactions committees dans le meme ordre (DAG)
- Chacun met a jour son registre localement de maniere deterministe
- Le registre est inclus dans les **snapshots** pour la synchronisation
- Resultat : tous les validators ont le meme registre

---

## Enregistrement

L'enregistrement passe par l'execution d'un pod.

### Cote pod (PodExecuteOutput)

Le pod declare les domaines a enregistrer dans un champ `registered_domains`
du `PodExecuteOutput`, au meme titre que `created_objects` ou `deleted_objects`.

Chaque entree contient :
- `name` : le nom de domaine (ex: `myapp.config`)
- `object_index` : index dans `created_objects` (pour un objet cree dans la meme tx)
- `object_id` : ObjectID direct (pour un objet existant, alternatif a `object_index`)

Le pod gere la logique metier : permissions namespace, formatting du nom, etc.

### Cote protocole (au commit)

Apres execution, le node :
1. Resout `object_index` en ObjectID reel (calcule apres execution)
2. Verifie l'unicite du domaine dans Pebble
3. Si collision : la transaction **revert**
4. Sinon : insert `domain_name -> ObjectID` dans Pebble

C'est le meme pattern que les conflits de version d'objets :
la validation se fait au commit, pas pendant l'execution.
Si le domaine est deja pris, la tx revert et le client reessaie.

---

## Transaction

Le header de la transaction contient :

```
created_objects_replication: [uint16]   // len = nb objets crees, valeur = replication
max_create_domains:          uint16     // nombre max de domaines enregistres
```

`len(created_objects_replication) > 0` force l'execution par tous les validators
(remplace l'ancien `creates_objects: true`). Les ObjectIDs des objets crees sont
calculables depuis le header : `hash(tx_id || index)`.

`max_create_domains` sert uniquement au calcul des frais. L'enregistrement
effectif est valide au commit. Si le pod produit plus de `max_create_domains`
enregistrements, la tx revert.

---

## Frais

L'enregistrement de domaines est inclus dans la formule de frais :

```
total = compute_fee * replication_ratio
      + nb_objets_standard_ATX * transit_fee
      + sum(replication_i / total_validators) * storage_fee
      + max_create_domains * domain_fee
```

Chaque objet cree paie un storage_fee pondere par sa propre replication
(lu depuis `created_objects_replication`).

`domain_fee` est eleve (10-100x le compute_fee) pour empecher le spam.
Pas de multiplicateur replication : le mapping est stocke dans Pebble sur tous
les validators, c'est un cout fixe.

Les mises a jour et suppressions de domaines ne paient que le compute_fee standard.

---

## Namespaces

Les domaines suivent une convention hierarchique avec `.` comme separateur.

- `system.*` : reserve au pod systeme (validators, params, fee_pool...)
- Autres : premier arrive, premier servi
- Une fois un namespace racine enregistre, seul le proprietaire peut y ajouter des sous-domaines

La logique de permissions vit dans le pod (system pod ou autre),
pas dans le protocole. Le protocole ne verifie que l'unicite.

---

## Mise a jour et suppression

- **Mise a jour** : le pod produit une entree `registered_domains` avec un nom existant et un nouvel ObjectID. Le protocole ecrase le mapping dans Pebble. Le pod verifie que l'appelant est le proprietaire du namespace.
- **Suppression** : le pod produit une entree avec un `object_id` vide. Le protocole supprime le mapping.

---

## Synchronisation

Le registre Pebble est inclus dans les snapshots :
- Un nouveau validator recoit le registre complet avec le snapshot
- Puis il traite les transactions committees depuis le snapshot
- Resultat : registre identique a celui des autres validators

---

## Resolution

La resolution est une operation **locale** :
- Lookup direct dans Pebble : `domain_name -> ObjectID`
- Pas de communication reseau
- Les SDK exposent une fonction de resolution cote client via l'API du node
