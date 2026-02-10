# Systeme Economique BluePods

Decisions prises pour le modele economique du reseau.

---

## Composantes des Frais

Chaque transaction paie quatre types de frais :

| Composante | Formule | Multiplicateur |
|---|---|---|
| Compute | `max_gas * gas_price` | `* replication_ratio` |
| Transit | fixe par objet standard dans l'ATX | Aucun |
| Storage | fixe par objet cree (forfait 4 KB) | `* effective_rep(replication_i) / total_validators` par objet |
| Domain | fixe par domaine enregistre | Aucun (cout fixe) |

### Replication Ratio et Effective Replication

`effective_rep` normalise la replication pour les formules de frais :

```
effective_rep(rep) = (rep == 0) ? total_validators : rep
```

Un singleton (replication=0) signifie "replique partout", donc `effective_rep(0) = total_validators`.
Cela unifie toutes les formules sans cas special.

`replication_ratio` est la proportion de validators qui executent la transaction.
Il est calcule comme l'union des holders de tous les MutableObjects :

```
executors = union(holders(mutable_obj_i) pour chaque mutable_obj_i)
replication_ratio = len(executors) / total_validators
```

- Si au moins un MutableObject est un singleton : `replication_ratio = 1.0`
- Si `created_objects_replication` ou `max_create_domains > 0` : `replication_ratio = 1.0`
- Sinon : union des holders via Rendezvous Hashing (cache par epoch)

Le calcul est deterministe : meme epoch, memes objets, meme resultat.
Les listes de holders sont deja calculees et cachees pour le routing.

### Champs du header pour les frais

Le header de la transaction contient :

```
max_gas:                     uint64     // gas max declare par le sender
created_objects_replication: [uint16]   // len = nb objets crees, valeur = replication
max_create_domains:          uint16     // nb max de domaines enregistres
```

- `max_gas` : le sender declare le gas maximum. Les frais de compute sont bases dessus.
  Un simple transfert declare peu de gas, une operation DeFi complexe beaucoup plus.
  Si l'execution depasse `max_gas`, la tx revert. Les frais sont deduits quoi qu'il arrive.
  Un `min_gas` protocole empeche le spam de txs a gas quasi-nul.
- `len(created_objects_replication) > 0` force l'execution par tous les validators
- Les ObjectIDs des objets crees sont calculables : `hash(tx_id || index)`
- Si l'execution cree plus d'objets que declare, la tx revert
- Si le pod produit plus de `max_create_domains` enregistrements, la tx revert

En pratique, les fonctions de pod ont un nombre d'objets et une replication previsibles
(`create_nft` = 1 objet a replication 10, `batch_mint` = N objets). C'est le contrat
de la fonction, connu par le SDK.

### Formule

```
total = max_gas * gas_price * replication_ratio
      + nb_objets_standard_ATX * transit_fee
      + sum(effective_rep(replication_i) / total_validators) * storage_fee
      + max_create_domains * domain_fee
```

- `nb_objets_standard_ATX` = nombre d'objets standard (non-singleton) dans ReadObjects + MutableObjects.
  Les singletons ne sont pas dans le body ATX, donc pas de transit.
- Chaque objet cree paie un storage_fee pondere par sa propre replication.
  Un singleton (`effective_rep = total_validators`, ratio=1.0) coute plus cher qu'un objet standard.

---

## Pourquoi les Frais sont au Niveau Protocole

Les frais sont deduits par le protocole, en dehors de l'execution du pod.

Si les frais passaient par le system pod :
- Le gas_coin est un singleton, donc tous les validators le stockent
- Tous devraient **executer** chaque tx pour deduire les frais
- Le system pod deviendrait un bottleneck sur 100% des transactions
- Le sharding d'execution serait detruit

Avec les frais au niveau protocole :
- Tous les validators **calculent** le fee depuis le header (arithmetique simple)
- Ils mettent a jour le gas_coin localement de maniere deterministe
- L'execution du pod reste shardee normalement (seuls les holders executent)

Calcul (depuis le header) ≠ execution (WASM dans le pod). Les melanger forcerait
tout le monde a executer tout le temps.

Meme logique pour la distribution epoch : c'est une operation implicite du consensus,
pas une transaction. Tous les validators arrivent au meme resultat car ils ont
les memes donnees (DAG, stakes, rounds produits).

---

## Pourquoi les Frais sont Bases sur max_gas (pas gas_used)

Le fee doit etre calculable uniquement depuis le header de la transaction, sans executer.

Le gas_coin est un singleton. Tous les validators stockent son solde et doivent s'accorder
sur le montant a deduire. Mais seuls les holders des mutable objects executent la transaction
(sharding d'execution). Les non-executeurs ne connaissent pas le gas_used.

Si le fee dependait du gas_used, soit tous les validators devraient executer (tuant le sharding),
soit les non-executeurs ne pourraient pas mettre a jour le gas_coin correctement.

Le `max_gas` declare par le sender est deterministe depuis le header. Tous les validators
peuvent calculer et deduire le meme montant independamment, sans executer. Les pods
plus legers declarent moins de gas et paient moins cher.

---

## Gas

- Le gas sert a la fois de mecanisme de securite et de pricing
- Le sender declare `max_gas` dans le header de la tx
- Les frais de compute = `max_gas * gas_price` (deduits quoi qu'il arrive, meme en cas de revert)
- Si l'execution depasse `max_gas`, la tx revert (frais toujours deduits)
- Un `min_gas` protocole empeche le spam de txs a gas quasi-nul
- Le gas metering est assure par l'instrumentation WASM (wasm-gas)

---

## Paiement

La Transaction contient un champ `gas_coin` separe des inputs metier.

Le gas_coin est un Coin singleton (replication=0) :
- Disponible localement sur tous les validators
- Pas dans le body ATX, pas d'attestation necessaire
- Ne compte pas dans la limite des 8 objets standard
- N'est PAS dans MutableObjects (sinon tous les validators executeraient chaque tx)

Les fees sont deduits au niveau protocole, en dehors de l'execution du pod.
Le pod n'a pas connaissance des frais.

### Modifications Implicites Protocole

Le gas_coin, le coin de l'agregateur et les reward_coins sont modifies par le protocole
(fee deduction, reward distribution) **sans incrementer leur version**.

Ces modifications sont des **operations implicites** : deterministes depuis le DAG,
appliquees par tous les validators dans le meme ordre. Elles ne participent pas au
conflict detection (pas dans MutableObjects).

Invariant : la version d'un objet track les modifications **explicites** via MutableObjects.
Les modifications implicites protocole changent le solde mais pas la version.

Consequence : un sender peut avoir plusieurs txs en vol simultanement sans conflit
de version sur son gas_coin. Les deductions s'appliquent sequentiellement en ordre DAG.

### Deduction des Frais

Le protocole deduit les frais **avant** l'execution, au moment du commit :

1. Calcule `total` depuis le header de la tx
2. Si `gas_coin.balance >= total` : deduit `total`, puis execute la tx
3. Si `gas_coin.balance < total` : deduit le solde restant, tx echoue (insufficient funds)
4. Si l'execution revert (gas limit, erreur logique) : les frais sont deja deduits, pas de remboursement

Les frais sont **toujours** deduits, meme si la tx echoue. L'inclusion dans le DAG a un cout.
L'agregateur fait un check best-effort du solde a la soumission (spam prevention, pas une garantie).

Les Coins restent des singletons. Avec 100 bytes par coin :
- 1M de coins = 100 MB par validator
- 10M de coins = 1 GB par validator
- Acceptable pour des machines datacenter

---

## Distribution des Frais

| Destination | Part | Moment |
|---|---|---|
| Agregateur | 20% | Immediat (credite au coin de l'agregateur) |
| Burn | 30% | Immediat (tokens detruits) |
| Epoch rewards | 50% | Accumule, distribue a chaque epoch |

Les frais sont deduits quoi qu'il arrive, meme si l'execution revert.
Le burn empeche les validators de manipuler les frais a leur profit et cree une pression deflationniste.

### Fee Summary dans les Vertices

Chaque vertex contient un `fee_summary` pre-calcule par l'agregateur :

```
fee_summary:
  total_fees:        uint64   // somme des frais de toutes les txs du vertex
  total_aggregator:  uint64   // somme des 20%
  total_burned:      uint64   // somme des 30%
  total_epoch:       uint64   // somme des 50%
```

A la reception d'un vertex, chaque validator verifie le `fee_summary` en recalculant
depuis les headers des txs. Si ca ne matche pas, le vertex est invalide.

Le `fee_summary` sert de cache pour l'epoch boundary : au lieu de re-parcourir toutes
les txs de l'epoch, on additionne les `total_epoch` de chaque vertex committe.

---

## Distribution des Rewards

A chaque epoch boundary, les rewards sont distribues aux validators de l'epoch (epochHolders).

### Reward Coin

Chaque validator possede un Coin singleton (`reward_coin`) cree a l'enregistrement.
Le `ValidatorInfo` stocke l'ObjectID de ce coin. La distribution epoch credite
directement ce coin.

### Calcul

```
epoch_fees   = sum(vertex.fee_summary.total_epoch) pour tous les vertices commites de l'epoch
reward_total = epoch_fees + issuance
```

La distribution est une operation protocole implicite (pas une transaction) :
- Tous les validators executent le meme calcul a la meme epoch boundary
- Les donnees sont identiques partout (stake, rounds dans le DAG, fee_summary des vertices)
- Pas besoin de transaction ni d'attestation
- Le protocole mint `reward_total` directement dans les reward_coins

### Formule

La formule combine le stake et la participation :

```
weight_i = stake_i * (rounds_produced_i / total_rounds_in_epoch)
share_i  = (weight_i / sum(all_weights)) * reward_total
```

- Proportionnelle au stake : incite au staking
- Reduite par la participation : un validator inactif gagne moins
- Penalite douce naturelle : pas besoin de slashing separe pour l'inactivite
- Les donnees sont deja disponibles (le DAG track les vertices produits par round)

---

## Storage et Suppression

Chaque objet contient un champ `fees` (metadata protocole, read-only pour le pod).

- A la creation : le protocole calcule le storage deposit et le stocke dans `object.fees` :
  `fees = storage_fee * effective_rep(replication) / total_validators`
- A la suppression : remboursement de 95% de `object.fees`, les 5% restants sont burned
- Le montant est fige a la creation. Independant des changements futurs de `storage_fee`
  ou `total_validators`.
- Le burn ratio empeche le spam creation/suppression
- Taille forfaitaire de 4 KB par objet (pas de calcul au byte)

---

## Inflation

- Taux initial : ~5% annuel
- Distribuee a chaque epoch avec les fees (epoch_fees + issuance = reward_total)
- Le protocole mint `reward_total` directement dans les reward_coins des validators
- Quand le burn (30% des fees) depasse l'issuance, le token devient deflationniste
- Taux stocke dans un singleton systeme, modifiable par governance

---

## Flow Complet

```
-- A chaque transaction (niveau protocole, avant execution) --

total = max_gas * gas_price * replication_ratio
      + nb_objets_standard_ATX * transit_fee
      + sum(effective_rep(replication_i) / total_validators) * storage_fee
      + max_create_domains * domain_fee

si gas_coin.balance < total:
  gas_coin.balance = 0                     // prend le reste
  tx echoue (insufficient funds)           // pas d'execution
sinon:
  gas_coin.balance -= total                // toujours deduit, meme si revert
  agregateur.coin  += total * 20%          // credit immediat (modification implicite)
                      total * 30%          // detruit (burn)
                      total * 50%          // comptabilise dans fee_summary du vertex

-- A chaque vertex (calcule par l'agregateur, verifie par tous) --

fee_summary.total_epoch = sum(total * 50%) pour chaque tx du vertex

-- A chaque epoch boundary (operation implicite du consensus) --

epoch_fees    = sum(vertex.fee_summary.total_epoch) pour tous les vertices de l'epoch
issuance      = total_supply * 5% / nb_epochs_par_an
reward_total  = epoch_fees + issuance

pour chaque validator in epochHolders:
  weight = stake * (rounds_produced / total_rounds)
  validator.reward_coin += (weight / sum_weights) * reward_total
```

---

## Constantes Protocole

Stockees dans un singleton systeme, modifiables par governance.

| Constante | Valeur initiale | Description |
|---|---|---|
| `gas_price` | a definir | Prix par unite de gas |
| `min_gas` | a definir | Gas minimum par transaction (anti-spam) |
| `transit_fee` | a definir | Prix fixe par objet standard dans l'ATX |
| `storage_fee` | a definir | Prix fixe par objet cree (forfait 4 KB) |
| `domain_fee` | a definir | Prix par domaine enregistre (10-100x d'une tx simple) |
| `aggregator_ratio` | 20% | Part des frais pour l'agregateur |
| `burn_ratio` | 30% | Part des frais burned |
| `epoch_ratio` | 50% | Part des frais pour les rewards d'epoch |
| `storage_refund_ratio` | 95% | Remboursement a la suppression d'un objet |
| `inflation_rate` | 5% annuel | Taux de creation de nouveaux tokens |

---

## Equilibre Naturel du Reseau

Le `replication_ratio` cree un mecanisme d'auto-regulation :

- Plus de validators → ratio plus bas → frais plus bas pour les users
- Frais plus bas → chaque validator gagne moins par tx
- Moins de revenus par validator → les non-rentables quittent
- Moins de validators → ratio remonte → frais remontent
- Le nombre de validators s'equilibre naturellement avec le volume de transactions

---

## Inspirations

- Solana : frais fixes par transaction, simplicite du pricing
- Sui : gas_coin separe, gestion du storage par depot, object model
- Ethereum EIP-1559 : burn pour anti-manipulation et pression deflationniste
- Le `replication_ratio` est specifique a BluePods et cree un equilibre naturel
  entre taille du reseau et revenus par validator
