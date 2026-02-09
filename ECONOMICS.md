# Systeme Economique BluePods

Decisions prises pour le modele economique du reseau.

---

## Composantes des Frais

Chaque transaction paie trois types de frais :

| Composante | Formule | `* replication_ratio` |
|---|---|---|
| Compute | fixe par tx | Oui |
| Transit | fixe par objet standard dans l'ATX | Non |
| Storage | fixe par objet cree (forfait 4 KB), base sur `max_creates` | Oui |

`replication_ratio` = proportion de validators qui executent ou stockent l'objet.

- `creates_objects=true` ou singleton dans MutableObjects : ratio = 1.0
- Objet standard : ratio = replication / total_validators
- Les singletons ne paient pas de transit (jamais dans le body ATX)

```
total = compute_fee * replication_ratio
      + nb_objets_standard_ATX * transit_fee
      + max_creates * storage_fee * replication_ratio
```

Le champ `max_creates` est declare dans le header de la transaction par le sender.
Si l'execution cree plus de `max_creates` objets, la transaction revert.
Si elle en cree moins, le sender paie quand meme pour `max_creates`.
En pratique, les fonctions de pod ont un nombre d'objets previsible
(`create_nft` = 1, `batch_mint` = N), donc c'est facile a remplir cote client.

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

## Pourquoi le Compute Fee est Fixe

Le fee doit etre calculable uniquement depuis le header de la transaction, sans executer.

Le gas_coin est un singleton. Tous les validators stockent son solde et doivent s'accorder
sur le montant a deduire. Mais seuls les holders des mutable objects executent la transaction
(sharding d'execution). Les non-executeurs ne connaissent pas le gas_used.

Si le fee dependait du gas_used, soit tous les validators devraient executer (tuant le sharding),
soit les non-executeurs ne pourraient pas mettre a jour le gas_coin correctement.

Un fee fixe par transaction est deterministe depuis le header. Tous les validators peuvent
calculer et deduire le meme montant independamment, sans executer.

---

## Gas

- Le gas est un mecanisme de securite uniquement, pas de pricing
- Le protocole impose un `gas_limit` fixe par transaction (constante reseau)
- Le sender ne specifie pas de gas_limit : c'est une constante du protocole
- Si l'execution depasse le gas_limit, la transaction revert
- Le compute fee est fixe par transaction, independant du gas consomme

---

## Paiement

La Transaction contient un champ `gas_coin` separe des inputs metier.

Le gas_coin est un Coin singleton (replication=0) :
- Disponible localement sur tous les validators
- Pas dans le body ATX, pas d'attestation necessaire
- Ne compte pas dans la limite des 8 objets standard
- N'est PAS dans MutableObjects (sinon tous les validators executeraient chaque tx)

Les fees sont deduits au niveau protocole, en dehors de l'execution du pod.
Le pod n'a pas connaissance des frais. Le protocole deduit le montant du gas_coin
apres le commit, de maniere deterministe depuis le header de la transaction.

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
| FeePool | 50% | Accumule, distribue a chaque epoch |

Le burn empeche les validators de manipuler les frais a leur profit et cree une pression deflationniste.

---

## Distribution du FeePool

A chaque epoch boundary, le FeePool est distribue aux validators de l'epoch (epochHolders).

### Reward Coin

Chaque validator possede un Coin singleton (`reward_coin`) cree a l'enregistrement.
Le `ValidatorInfo` stocke l'ObjectID de ce coin. La distribution epoch credite
directement ce coin.

### Execution

La distribution est une operation protocole implicite (pas une transaction) :
- Tous les validators executent le meme calcul a la meme epoch boundary
- Les donnees sont identiques partout (stake, rounds dans le DAG)
- Pas besoin de transaction ni d'attestation
- C'est une operation implicite du consensus, comme le passage d'epoch

### Formule

La formule combine le stake et la participation :

```
weight_i = stake_i * (rounds_produced_i / total_rounds_in_epoch)
share_i  = (weight_i / sum(all_weights)) * fee_pool
```

- Proportionnelle au stake : incite au staking
- Reduite par la participation : un validator inactif gagne moins
- Penalite douce naturelle : pas besoin de slashing separe pour l'inactivite
- Les donnees sont deja disponibles (le DAG track les vertices produits par round)

---

## Storage et Suppression

- A la creation d'un objet : depot de `storage_fee * replication_ratio` (base sur `max_creates`)
- A la suppression d'un objet : remboursement de 95%, les 5% restants sont burned
- Le burn ratio empeche le spam creation/suppression
- Taille forfaitaire de 4 KB par objet (pas de calcul au byte)

---

## Inflation

- Taux initial : ~5% annuel
- Distribuee a chaque epoch dans le FeePool
- Les validators recoivent `fees_epoch + issuance_epoch` en un seul versement
- Quand le burn depasse l'issuance, le token devient deflationniste
- Taux stocke dans un singleton systeme, modifiable par governance

---

## Flow Complet

```
-- A chaque transaction (niveau protocole, pas d'execution WASM) --

total = compute_fee * replication_ratio
      + nb_objets_standard_ATX * transit_fee
      + max_creates * storage_fee * replication_ratio

sender.gas_coin  -= total
agregateur.coin  += total * 20%
[burned]         += total * 30%
fee_pool         += total * 50%

-- A chaque epoch boundary (operation implicite du consensus) --

issuance  = total_supply * 5% / nb_epochs_par_an
fee_pool += issuance

pour chaque validator in epochHolders:
  weight = stake * (rounds_produced / total_rounds)
  validator.reward_coin += (weight / sum_weights) * fee_pool

fee_pool = 0
```

---

## Constantes Protocole

Stockees dans un singleton systeme, modifiables par governance.

| Constante | Valeur initiale | Description |
|---|---|---|
| `compute_fee` | a definir | Prix fixe par transaction |
| `transit_fee` | a definir | Prix fixe par objet standard dans l'ATX |
| `storage_fee` | a definir | Prix fixe par objet cree (forfait 4 KB) |
| `aggregator_ratio` | 20% | Part des frais pour l'agregateur |
| `burn_ratio` | 30% | Part des frais burned |
| `pool_ratio` | 50% | Part des frais pour le FeePool |
| `storage_refund_ratio` | 95% | Remboursement a la suppression d'un objet |
| `inflation_rate` | 5% annuel | Taux de creation de nouveaux tokens |
| `gas_limit` | a definir | Plafond de gas par transaction (constante reseau) |

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
