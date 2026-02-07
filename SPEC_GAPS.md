# Ecarts avec bluepods_v2.md

Liste des simplifications et ecarts entre l'implementation actuelle et la spec `bluepods_v2.md`.

---

## 1. ~~Singletons inclus dans le corps des transactions~~ RESOLU

**Spec** : "Les singletons ne sont pas inclus dans le corps de la transaction car chaque validator possede deja leur contenu localement. Seuls leur identifiant et version attendue apparaissent dans le header."

**Fix applique** : Le client n'inclut plus les singletons (replication=0) dans le vecteur Objects de l'ATX. Le noeud resout les objets manquants depuis son state local via `resolveMutableObjects()`.

---

## 2. ReadObjects non utilise

**Spec** : Deux listes d'objets dans le header : `ReadObjects` (version verifiee, pas incrementee) et `MutableObjects` (version verifiee ET incrementee).

**Actuel** : Le client ne remplit jamais `ReadObjects`. Toutes les references passent par `MutableObjects`.

**Impact** : Aucun pour l'instant (split/transfer ne touchent qu'un objet mutable). Deviendra important pour des transactions multi-objets ou un objet est lu sans etre modifie.

**Fix** : Ajouter le support `ReadObjects` dans le client quand un pod en aura besoin.

---

## 3. ~~Version state vs version tracker (fragile)~~ RESOLU

**Spec** : "La version s'incremente des qu'un objet est declare comme mutable dans une transaction reussie, independamment du fait que son contenu change effectivement ou non."

**Fix applique** : `state.Execute` appelle `ensureMutableVersions()` apres `processOutput()`. Cette fonction parcourt tous les objets dans `MutableObjects` et garantit que leur version est incrementee a `expectedVersion+1`, meme si le pod ne les a pas retournes dans `UpdatedObjects`.

---

## 4. Pas d'attestation BLS / collecte de quorum

**Spec** : L'agregateur collecte les attestations des holders via QUIC, agrege les signatures BLS, et inclut les preuves dans le vertex. Le vecteur `Proofs` de l'ATX contient la signature BLS agregee + bitmap.

**Actuel** : Le vecteur `Proofs` est toujours vide. Toutes les transactions sont acceptees sans preuve de quorum. Pas de collecte d'attestations, pas de BLS.

**Impact** : Aucune securite sur l'authenticite des objets inclus dans les transactions. Un client malveillant peut inclure de faux objets.

---

## 5. Pas de sharding d'execution

**Spec** : "Chaque holder d'au moins un objet dans MutableObjects appelle execute(tx)." Seuls les holders executent.

**Actuel** : Tous les validators executent toutes les transactions.

**Impact** : Pas de scaling horizontal. Chaque validator supporte la charge totale du reseau.

---

## 6. Pas de limites de transaction enforçees

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

## 7. Pas de Rendezvous Hashing

**Spec** : Les holders d'un objet sont determines par Rendezvous Hashing. Seuls les N holders stockent l'objet.

**Actuel** : Tous les validators stockent tous les objets (tout est traite comme singleton).

**Impact** : Pas de distribution du stockage. Chaque noeud stocke l'integralite du state.

---

## Priorite suggeree

1. ~~**Point 3** (version state/tracker)~~ RESOLU
2. **Point 6** (limites tx) - Securite basique
3. ~~**Point 1** (singletons dans ATX)~~ RESOLU
4. **Point 2** (ReadObjects) - Quand un pod en aura besoin
5. **Points 4, 5, 7** - Architecture reseau, gros chantiers
