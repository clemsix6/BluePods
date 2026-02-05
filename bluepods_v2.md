# Architecture Technique de la Blockchain

## Introduction et Objectifs

Cette blockchain vise une finalité de l'ordre de 400 millisecondes tout en supportant plusieurs milliers de validators. Elle combine un modèle de données orienté objets avec un consensus basé sur un graphe acyclique dirigé et une propagation hybride associant gossip et connexions directes.

Le state du réseau est fragmenté en objets indépendants, chacun répliqué sur un sous-ensemble de validators appelés holders. Le consensus ne nécessite pas que chaque validator possède l'intégralité du state. La propagation des messages utilise un protocole de gossip pour les données globales et des connexions QUIC directes pour la collecte ciblée des attestations.

Le réseau cible un profil de validators élitiste avec des machines puissantes disposant d'excellentes connexions réseau. Ce choix permet d'optimiser les performances tout en maintenant une décentralisation significative grâce au grand nombre de validators supportés.

---

## Modèle de Données : Les Objets

Le state global du réseau est découpé en objets. Chaque objet est une unité de données autonome possédant un identifiant unique de 32 bytes qui reste immuable sur toute la durée de vie de l'objet. Cet identifiant est accompagné d'une version, un entier non signé qui s'incrémente à chaque modification. L'objet contient également une référence à son type, qui pointe vers le pod et la structure de données définissant son schéma, ainsi que le contenu sérialisé lui-même, limité à 4 KB maximum.

Chaque objet possède un facteur de réplication qui détermine sa distribution sur le réseau. Une valeur de 0 indique un singleton, répliqué sur tous les validators. Une valeur supérieure ou égale à 10 indique un objet standard, répliqué sur ce nombre de holders via Rendezvous Hashing. Le minimum pour un objet non-singleton est de 10 holders, garantissant un quorum de 67% atteignable avec 7 holders, une tolérance à 3 pannes simultanées, et une distribution suffisante pour éviter la collusion. Le facteur de réplication est immutable après création.

La version permet de détecter les conflits de concurrence. Deux transactions déclarant le même objet dans leur liste MutableObjects avec la même version attendue sont en conflit, et seule une des deux pourra être validée. Ce mécanisme garantit la cohérence du state sans nécessiter de locks globaux. La version s'incrémente dès qu'un objet est déclaré comme mutable dans une transaction réussie, indépendamment du fait que son contenu change effectivement ou non.

La limite de 4 KB par objet couvre la grande majorité des cas d'usage : un wallet avec ses métadonnées tient en quelques centaines de bytes, un NFT avec son URI et ses attributs en 1-2 KB, une position DeFi en moins de 1 KB. Cette limite permet d'inclure les objets complets dans les transactions, simplifiant considérablement le modèle d'exécution. Pour les données volumineuses dépassant cette limite, un système de stockage off-chain avec erasure coding sera développé séparément, où seules les métadonnées et certificats de disponibilité seront stockés on-chain.

### Objets et Singletons

Un objet standard possède un facteur de réplication configurable, typiquement entre 10 et 100 holders. Par défaut, un objet est créé avec le minimum de 10 holders, déterminés par le Rendezvous Hashing. Ce facteur peut être augmenté selon les besoins de disponibilité de l'application.

Un singleton est un objet dont le facteur de réplication est 0, indiquant une réplication sur l'intégralité du réseau. Chaque validator actif doit posséder une copie d'un singleton. Cette réplication totale garantit une persistance maximale et une disponibilité immédiate pour tous les participants du réseau.

Les singletons sont utilisés pour les données dont la persistance et la disponibilité universelle sont critiques. Tous les pods sont obligatoirement des singletons car leur code doit être accessible par n'importe quel validator pour exécuter les transactions. Les objets système contenant des informations devant être connues de tous, comme la liste des validators actifs ou les paramètres du protocole, sont également des singletons.

Les singletons bénéficient d'optimisations spécifiques. Ils ne sont pas inclus dans le corps de la transaction car chaque validator possède déjà leur contenu localement. Seuls leur identifiant et version attendue apparaissent dans le header. Cette optimisation réduit significativement la bande passante et élimine le besoin de collecter des attestations pour les singletons.

Les développeurs peuvent créer leurs propres singletons pour des cas d'usage nécessitant une garantie de disponibilité maximale. Cependant, les frais de création et de modification d'un singleton sont significativement plus élevés que pour un objet standard, reflétant le coût de réplication sur l'ensemble du réseau.

---

## Système de Noms de Domaine

### Principe et Motivation

Le réseau propose un système de noms de domaine permettant d'associer des identifiants lisibles à des adresses d'objets. Plutôt que de manipuler des identifiants de 32 bytes comme `0x7a3f8b2c...`, les développeurs et utilisateurs peuvent référencer des objets via des noms comme `system.validators` ou `myapp.config`.

Ce système facilite la découverte des objets système, améliore l'ergonomie des SDK, et permet aux applications de publier des points d'entrée connus sans nécessiter de documentation externe pour les adresses.

### Le DomainRegistry

La résolution des domaines repose sur un singleton dédié appelé DomainRegistry. Ce singleton maintient une table de correspondance entre les noms de domaine et les identifiants d'objets.

Le DomainRegistry constitue une exception à la limite de 4 KB imposée aux objets. Cette exception est justifiée par la nature des singletons : ils ne sont jamais inclus dans le corps des transactions puisque tous les validators les possèdent déjà localement. La limite de 4 KB existe principalement pour contraindre la taille des transactions et la bande passante lors de la collecte d'attestations, deux contraintes qui ne s'appliquent pas aux singletons. Le DomainRegistry peut ainsi croître selon les besoins du réseau.

Avec une taille moyenne de 80 bytes par entrée (nom de domaine plus identifiant cible), le registry peut contenir 10 000 domaines pour environ 800 KB, ou 100 000 domaines pour environ 8 MB. Ces volumes restent gérables pour un singleton répliqué sur tous les validators.

### Organisation des Namespaces

Les domaines suivent une convention de nommage hiérarchique utilisant le point comme séparateur. Le premier segment identifie le namespace, les segments suivants précisent la ressource.

Le namespace `system` est réservé aux objets fondamentaux du protocole. Il contient les références vers la liste des validators actifs, les paramètres du réseau, et autres données critiques pour le fonctionnement de la blockchain. Seul le pod système peut enregistrer des domaines dans ce namespace.

Les autres namespaces sont ouverts aux développeurs selon une politique de premier arrivé, premier servi. Un développeur peut enregistrer `myapp.users` ou `defi.pools` pour exposer les points d'entrée de son application. Une fois un namespace racine enregistré par une entité, seule cette entité peut y ajouter de nouveaux sous-domaines.

### Enregistrement et Résolution

L'enregistrement d'un nouveau domaine s'effectue via une transaction appelant le pod système. La transaction spécifie le nom de domaine souhaité et l'identifiant de l'objet cible. Le pod système vérifie que le domaine n'est pas déjà pris et que l'appelant a l'autorité pour enregistrer dans le namespace concerné, puis met à jour le DomainRegistry.

La résolution est une opération locale. Chaque validator possède le DomainRegistry en tant que singleton et peut résoudre n'importe quel domaine instantanément sans communication réseau. Les SDK exposent une fonction de résolution qui interroge le state local du validator.

Un domaine peut être mis à jour pour pointer vers un nouvel objet, ou supprimé par son propriétaire. Ces opérations suivent le même processus que l'enregistrement initial.

### Prévention du Spam

Pour éviter que des acteurs malveillants ne saturent le DomainRegistry en enregistrant massivement des domaines, les frais d'enregistrement sont significativement plus élevés que ceux d'une transaction standard. Ce coût dissuasif, de l'ordre de 10 à 100 fois le prix d'une transaction normale, garantit que seuls les domaines réellement utiles sont enregistrés.

Les frais de mise à jour et de suppression restent modérés car ces opérations ne font pas croître le registry.

---

## Distribution du Stockage par Rendezvous Hashing

Pour les objets standards, contrairement aux architectures où tous les validators stockent l'intégralité du state, chaque objet n'est répliqué que sur un sous-ensemble de validators appelés holders. Le nombre de holders est défini par le facteur de réplication de l'objet, configurable entre 10 et plusieurs centaines selon les besoins de disponibilité. Ces holders sont déterminés par un algorithme de Rendezvous Hashing. Les singletons échappent à cette règle car ils sont répliqués sur tous les validators du réseau.

Pour chaque objet, on calcule le hash de l'identifiant de l'objet combiné avec l'identifiant de chaque validator actif. On obtient ainsi un score pour chaque validator relativement à cet objet. On trie les validators par score décroissant et on prend les N premiers, où N est le facteur de réplication de l'objet. Ces N validators sont les holders de cet objet et ont la responsabilité de le stocker, de voter sur les transactions le concernant, et d'exécuter les modifications.

L'avantage du Rendezvous Hashing par rapport à un simple modulo sur l'identifiant est la stabilité lors des changements de composition du réseau. Quand un validator rejoint ou quitte le réseau, seule une fraction des objets doit être redistribuée. Si un validator disparaît, seul le (N+1)ème dans le classement de chaque objet concerné devient nouveau holder. Les N-1 autres holders restent inchangés. Cette propriété minimise le reshuffling et le trafic de synchronisation lors des changements d'epoch.

Tout participant au réseau peut calculer indépendamment la liste des holders de n'importe quel objet. Il suffit de connaître l'identifiant de l'objet, son facteur de réplication, et la liste des validators actifs de l'epoch courante. Le calcul est purement déterministe et ne nécessite aucune communication. Quand une transaction arrive, n'importe quel validator peut déterminer quels sont les holders attendus pour voter, et donc calculer si le quorum est atteint.

Avec un facteur de réplication de 50, le réseau reste résilient même si 16 holders tombent simultanément pour un objet donné. Le quorum de 67% peut toujours être atteint avec les 34 holders restants. Les applications critiques peuvent opter pour un facteur plus élevé, tandis que les données moins critiques peuvent utiliser le minimum de 10 holders pour réduire les coûts.

---

## Gestion des Validators Actifs

### Organisation en Epochs

Le réseau fonctionne par epochs d'une durée de plusieurs heures. Au début de chaque epoch, la liste des validators actifs est figée dans un snapshot. Ce snapshot détermine quels validators participent au consensus et au stockage pour toute la durée de l'epoch. Les changements de composition, comme l'arrivée de nouveaux validators ou le départ de validators existants, ne prennent effet qu'au passage à l'epoch suivante. Cette stabilité simplifie considérablement le raisonnement sur le système pendant une epoch donnée.

### Détection des Validators Inactifs par Observation des Votes

Pendant une epoch, certains validators peuvent tomber en panne ou perdre leur connectivité. Le système doit détecter ces situations pour éviter que des transactions restent bloquées en attendant des votes qui ne viendront jamais.

La détection repose sur l'observation du comportement de vote des validators, sans nécessiter de protocole heartbeat dédié. Chaque validator est holder d'un certain nombre d'objets calculable via le Rendezvous Hashing. Quand des transactions touchent ces objets, le validator est censé voter. Son absence de vote sur des transactions où il était attendu constitue un signal d'inactivité.

Au niveau intra-epoch, si un validator n'a émis aucun vote sur les transactions le concernant pendant une période de 30 secondes alors que d'autres transactions touchant ses objets ont circulé, les autres validators le marquent localement comme probablement inactif. Ce marquage n'est pas une exclusion formelle mais permet d'ajuster les attentes de quorum temporairement. Les autres validators savent qu'ils ne doivent pas attendre son vote indéfiniment et peuvent considérer le quorum atteint sans lui si suffisamment d'autres holders ont voté.

Ce mécanisme n'ajoute aucun overhead réseau. Les validators observent simplement le flux normal de votes et constatent les absences. Le silence là où on attendait une participation est le signal.

### Exclusion et Pénalités en Fin d'Epoch

À la fin de chaque epoch, le protocole calcule des statistiques de participation pour chaque validator. Pour chaque validator, on compte le nombre de transactions où il était holder attendu d'au moins un objet impliqué, et parmi celles-ci, combien ont reçu son vote.

Si le taux de participation d'un validator descend sous 67%, il reçoit une pénalité progressive sur son stake. Cette pénalité augmente si le comportement persiste sur plusieurs epochs consécutives. Si le taux de participation reste sous 33% pendant plusieurs epochs consécutives, le validator est exclu du snapshot de l'epoch suivante. Il ne participera plus au consensus ni au stockage jusqu'à ce qu'il démontre sa disponibilité et soit réintégré.

Un validator exclu peut revenir à l'epoch suivante s'il se reconnecte et passe les vérifications d'admission. L'exclusion n'est pas permanente mais reflète l'incapacité momentanée à remplir ses obligations.

---

## Structure des Transactions

### Anatomie d'une Transaction

Une transaction est un message signé demandant l'exécution d'une opération sur le state. Sa taille maximale est de 48 KB. Elle se compose de plusieurs sections.

Le header contient les métadonnées essentielles. On y trouve l'identifiant unique de la transaction, calculé comme le hash de son contenu canonique. Le header déclare deux listes d'objets. La première liste contient les objets en lecture seule (ReadObjects), dont la version est vérifiée mais ne change pas. La seconde liste contient les objets mutables (MutableObjects), dont la version sera incrémentée si la transaction réussit. Cette séparation explicite permet à tout validator de déduire les changements de version directement depuis le header, sans exécuter la transaction.

Les limites par transaction sont de 8 objets standards maximum et 32 singletons maximum. Les singletons ne comptent pas dans la limite des 8 objets car ils ne sont pas inclus dans le corps de la transaction. Cette limite sur les objets standards existe pour contraindre la taille du corps de la transaction et la complexité de la collecte d'attestations. Les singletons n'impactent ni l'un ni l'autre puisque chaque validator les possède déjà localement.

Le header contient également un champ booléen `creates_objects` qui indique si la transaction peut créer de nouveaux objets. Si ce flag est activé, la transaction est exécutée par tous les validators du réseau (comme pour les singletons) afin que chacun puisse calculer de manière déterministe le contenu des objets créés.

La section d'invocation spécifie le pod à appeler, le nom de la fonction, et les arguments sérialisés. Les arguments peuvent inclure des références aux objets déclarés dans le header ainsi que des valeurs scalaires.

La section des objets contient uniquement les objets standards référencés par la transaction, collectés et attestés par les holders lors de la phase d'agrégation. Les singletons ne sont pas inclus car tous les validators les possèdent déjà. Ces objets standards sont inclus dans la transaction finale avec leurs preuves de quorum.

La section des signatures contient une ou plusieurs signatures autorisant la transaction.

### Objets Inclus dans la Transaction

Les objets standards complets sont inclus directement dans la transaction plutôt que d'extraire uniquement certains champs. Les singletons, en revanche, ne sont pas inclus car chaque validator les possède déjà localement. Cette approche simplifie considérablement le modèle d'exécution en adoptant un paradigme classique similaire à Ethereum ou Solana : la fonction d'exécution reçoit tous les objets en entrée et retourne les nouveaux états en sortie.

Avec la limite de 4 KB par objet et un maximum de 8 objets standards par transaction, la charge maximale en objets est de 32 KB. En ajoutant le header, l'invocation et les signatures, la transaction complète reste sous la limite de 48 KB. Les singletons référencés n'ajoutent que leur identifiant et version dans le header, économisant significativement la bande passante.

Cette architecture offre plusieurs avantages. Le modèle mental est simple : une transaction contient tout ce qu'il faut pour son exécution. Les développeurs de pods n'ont pas à gérer d'extraction partielle de données. La vérification est directe : on peut valider que hash(objet) correspond au hash attesté par le quorum. Tout validator qui doit exécuter la transaction dispose de toutes les données nécessaires, soit incluses dans la transaction pour les objets standards, soit disponibles localement pour les singletons.

La contrepartie est une augmentation de la bande passante par rapport à une approche d'extraction minimale. Ce compromis est acceptable car la limite de 4 KB par objet garde les transactions raisonnables, et l'architecture de sharding horizontal permet au réseau de scaler malgré cette charge accrue. L'exclusion des singletons du corps de la transaction atténue également ce coût.

---

## Les Pods (Smart Contracts)

Les smart contracts s'appellent des pods. Chaque pod est obligatoirement un singleton, répliqué sur l'ensemble des validators du réseau. Cette contrainte garantit que tout validator peut exécuter n'importe quelle transaction sans avoir à récupérer le code du pod auprès d'autres validators.

### Fonction Execute

Chaque pod expose une fonction execute qui constitue le cœur de la logique métier. Cette fonction prend en entrée la transaction complète incluant tous les objets référencés, et retourne les nouveaux états des objets modifiés.

Ce modèle est similaire aux smart contracts traditionnels comme sur Ethereum ou Solana. La fonction reçoit toutes les données nécessaires en entrée et produit les modifications en sortie. Le développeur n'a pas à se soucier de quels objets sont disponibles localement car la transaction contient tous les objets attestés par le quorum.

L'exécution reste distribuée grâce au sharding horizontal. Chaque holder d'un objet déclaré dans MutableObjects exécute la transaction et calcule le nouvel état de ses objets. Puisque tous les holders reçoivent les mêmes entrées via la transaction, ils calculent tous le même résultat de façon déterministe.

### Transactions sur Singletons Uniquement

Les transactions qui impliquent exclusivement des singletons bénéficient d'une optimisation majeure. Puisque tous les validators possèdent tous les singletons, il n'y a pas besoin de phase de collecte distribuée des objets. Chaque validator peut directement exécuter la transaction localement avec l'ensemble des données nécessaires.

Cette optimisation élimine les étapes de collecte des attestations. La transaction est directement incluse dans un vertex puis propagée via gossip. Le temps de traitement est significativement réduit car seul le consensus sur l'ordre des transactions reste nécessaire.

Les transactions système comme les opérations de staking, les mises à jour de paramètres du protocole ou les interactions avec le pod système bénéficient naturellement de cette optimisation puisqu'elles manipulent principalement des singletons.

### Résultats d'Exécution

L'exécution d'une transaction peut produire trois types de résultats. Premièrement, des objets modifiés : tout objet déclaré dans la liste MutableObjects voit sa version incrémentée, même si son contenu n'est pas effectivement modifié par la logique du pod. Cette règle garantit que le versioning est déterministe et déductible du header de la transaction sans exécution. Deuxièmement, des nouveaux objets créés : de nouveaux identifiants apparaissent dans le state, avec une limite de 16 objets créés maximum par transaction. Troisièmement, des objets supprimés : un objet dans MutableObjects peut être supprimé par la logique du pod, auquel cas il est retiré du state.

Cette sémantique de versioning place la responsabilité sur le développeur : un objet ne doit être dans MutableObjects que s'il est susceptible d'être modifié. Placer systématiquement tous les objets en MutableObjects "par précaution" augmenterait artificiellement les conflits de version.

---

## Le Consensus DAG (Mysticeti v2)

### Principe du Graphe Acyclique Dirigé

Le consensus utilise un graphe acyclique dirigé, ou DAG, pour ordonner les transactions et atteindre la finalité. Contrairement à une blockchain linéaire où chaque bloc pointe vers un seul bloc précédent, dans un DAG chaque unité de données peut pointer vers plusieurs unités précédentes.

L'unité de base du DAG s'appelle un vertex. Chaque validator produit des vertices et les propage au réseau. Un vertex contient des références à des vertices précédents, appelées liens parents, ce qui forme la structure de graphe. Le DAG permet un parallélisme naturel : plusieurs validators peuvent produire des vertices simultanément sans conflit, tant que ces vertices finissent par être reliés dans le graphe.

Le design du DAG est leaderless. Il n'y a pas de leader désigné pour proposer les blocs ou coordonner le consensus. Tous les validators produisent des vertices en parallèle sans coordination centralisée. Cette approche élimine les bottlenecks et permet un débit maximal sans être bridé par la performance d'un leader unique.

### Structure d'un Vertex

Un vertex est produit par un validator spécifique et contient plusieurs éléments. Il contient les transactions complètes que ce validator propose d'inclure, chacune accompagnée de ses objets attestés et de sa preuve de quorum. Chaque preuve de quorum comprend une signature BLS agrégée des holders ayant attesté les objets, avec un bitmap indiquant quels holders ont signé. Le vertex contient également les liens vers les vertices parents dans le DAG, c'est-à-dire les vertices que ce validator a observés avant de produire le sien. Il contient enfin la signature du validator producteur.

Les vertices sont compressés avec zstd avant propagation sur le réseau, offrant un ratio de compression d'environ 2x en moyenne.

### Règles de Validité d'un Vertex

Pour qu'un vertex soit considéré valide, il doit respecter plusieurs règles. Ses liens parents doivent pointer vers des vertices existants et déjà validés. Le validator producteur doit être dans la liste des validators actifs de l'epoch courante. La signature doit être valide. Les preuves de quorum doivent être vérifiables : la signature BLS agrégée doit être valide pour les holders indiqués dans le bitmap, et ces holders doivent représenter au moins 67% des holders attendus pour chaque objet de la transaction.

### Progression du DAG et Rounds

Le DAG progresse par rounds successifs. À chaque round, les validators produisent de nouveaux vertices qui référencent des vertices du round précédent. Un vertex du round N doit inclure des liens vers des vertices du round N-1 provenant d'au moins 67% des validators pondéré par le stake.

Cette règle de quorum sur les liens parents garantit que le DAG ne peut pas se fragmenter. Si un validator produit un vertex qui ne référence pas suffisamment de vertices du round précédent, son vertex sera considéré invalide et ignoré par le reste du réseau.

### Commit Rule et Finalité

La règle de commit détermine quand une transaction est considérée comme finale et irréversible. Un vertex est committé quand il est référencé directement ou indirectement par des vertices de rounds ultérieurs produits par au moins 67% des validators.

Concrètement, si un vertex V du round N est inclus dans les liens parents directs ou transitifs de vertices du round N+2 produits par un quorum de validators, alors V est committé. Toutes les transactions référencées par V sont alors finales.

Cette règle offre une finalité rapide car elle ne nécessite que 2 rounds de propagation après la production du vertex initial.

### Gestion des Conflits

Quand deux transactions déclarent le même objet dans leur liste MutableObjects avec la même version attendue, elles sont en conflit. Le DAG détermine laquelle sera exécutée et laquelle sera rejetée.

L'ordre des transactions est déterminé par leur position dans le DAG. Quand un vertex est committé, toutes les transactions qu'il contient sont ordonnées de manière déterministe. Si deux transactions conflictuelles se trouvent dans des vertices différents, l'ordre de commit de ces vertices détermine laquelle est exécutée en premier. La seconde transaction, attendant une version qui ne correspond plus à la version courante, sera rejetée pour conflit de version.

Si deux transactions conflictuelles se trouvent dans le même vertex, une règle de tri déterministe comme l'ordre lexicographique des hashes détermine laquelle est prioritaire.

La détection des conflits ne nécessite pas d'exécution. Chaque validator peut calculer la version courante de n'importe quel objet en suivant l'historique des transactions committées dans le DAG. Pour chaque transaction committée qui déclare un objet dans MutableObjects, la version de cet objet est incrémentée. La version attendue par une nouvelle transaction est comparée à cette version calculée pour détecter les conflits.

En cas de conflit de version, la transaction rejetée retourne une erreur au client. Le client ou le wallet peut alors automatiquement re-soumettre la transaction avec la version mise à jour de l'objet. Ce comportement est similaire à celui de Sui et représente un compromis acceptable pour un MVP.

### Suivi Global des Versions

Le DAG constitue la source de vérité pour le versioning de tous les objets du réseau. Chaque validator, qu'il soit holder ou non d'un objet donné, peut calculer la version courante de cet objet en suivant l'historique des transactions committées.

L'algorithme est simple : pour chaque transaction committée dans l'ordre du DAG, on examine sa liste MutableObjects. Chaque objet présent dans cette liste voit sa version incrémentée de 1. Les objets dans ReadObjects ne changent pas de version. Ce calcul est purement déterministe et ne nécessite aucune exécution de la logique des pods.

Cette propriété est fondamentale pour l'atomicité des transactions multi-objets. Considérons une transaction TX1 qui modifie les objets O1 et O2. Si une transaction TX2 modifie O2 et commit avant TX1, alors quand TX1 tente de s'exécuter, la version de O2 ne correspond plus. Tous les validators peuvent détecter ce conflit indépendamment, sans coordination, simplement en calculant les versions depuis le DAG.

Le coût de ce suivi est minimal. Les validators ne stockent pas le contenu des objets qu'ils ne détiennent pas, seulement un mapping identifiant vers version. Avec 40 bytes par objet (32 bytes d'identifiant + 8 bytes de version), un million d'objets représente 40 MB de mémoire. Ce tracking léger permet le scaling horizontal : les non-holders ne participent pas à l'exécution mais peuvent vérifier la cohérence des versions pour garantir l'atomicité globale.

---

## Architecture Réseau

### Connexions QUIC Permanentes

Chaque validator maintient une connexion QUIC persistante avec tous les autres validators du réseau. Avec 5000 validators, cela représente environ 5000 connexions par validator. Cette architecture en mesh complet permet une communication directe à faible latence entre n'importe quelle paire de validators.

Les connexions QUIC offrent plusieurs avantages. Le multiplexage de streams permet d'envoyer plusieurs requêtes en parallèle sur une même connexion sans head-of-line blocking. La persistance des connexions élimine le coût des handshakes répétés. Le chiffrement TLS 1.3 intégré assure la sécurité des communications.

Sur des machines puissantes avec 64 GB de RAM ou plus, le coût mémoire d'environ 250 MB pour maintenir 5000 connexions est négligeable.

### Séparation des Flux de Communication

Le réseau utilise deux mécanismes de communication distincts selon le type de données.

Les connexions QUIC directes sont utilisées pour la collecte des attestations. Quand un validator agrège les votes pour une transaction, il contacte directement les holders concernés via QUIC. Cette communication ciblée ne sollicite que les holders de chaque objet standard (selon leur facteur de réplication), pas l'ensemble du réseau. Les singletons ne nécessitent pas de collecte. La latence est minimale car il n'y a pas de hops intermédiaires.

Le protocole de gossip est utilisé pour propager les vertices à tout le réseau. Chaque vertex contient les transactions complètes avec leurs objets attestés et leurs preuves de quorum.

### Gossip des Vertices

Le broadcast direct, où chaque validator envoie directement ses messages à tous les autres, génère un nombre de messages proportionnel au carré du nombre de validators. Avec 200 validators, cela représente 40 000 messages par broadcast. Avec 2000 validators, cela représenterait 4 millions de messages, ce qui est intenable.

Le gossip résout ce problème de scaling. Chaque validator n'envoie ses messages qu'à un petit nombre de pairs, appelé fanout. Ces pairs relaient ensuite le message à leurs propres pairs, et ainsi de suite jusqu'à ce que tout le réseau soit couvert. La complexité passe de O(n²) à O(n log n).

Le fanout choisi est d'environ 40. Chaque validator envoie son message à 40 pairs choisis. Avec un fanout de 40, le réseau de plusieurs milliers de validators est couvert en 3 hops environ. Le premier hop atteint 40 validators, le deuxième hop atteint 1600 validators, le troisième hop couvre le reste.

Les vertices contiennent les transactions complètes avec leurs objets attestés et leurs preuves de quorum. Ils sont compressés avec zstd avant propagation, offrant un ratio de compression d'environ 2x en moyenne.

---

## Système de Collecte et Attestation des Données

### Principe de la Collecte Directe

Au lieu de propager les votes par gossip à travers tout le réseau, le système utilise une collecte directe et ciblée. Quand un validator reçoit une transaction d'un utilisateur, il devient l'agrégateur pour cette transaction. L'agrégateur contacte directement les holders des objets concernés via les connexions QUIC permanentes pour collecter leurs attestations.

Cette approche offre plusieurs avantages. La latence est minimale car la communication est directe sans hops intermédiaires. La charge réseau est réduite car seuls les holders concernés sont sollicités, pas l'ensemble du réseau. Le parallélisme est maximal car l'agrégateur contacte tous les holders simultanément.

### Rôle de l'Agrégateur

L'agrégateur est le validator qui reçoit la transaction de l'utilisateur. Son rôle est de collecter les attestations des holders, vérifier que le quorum est atteint, assembler la preuve de quorum, et inclure la transaction dans un vertex.

L'agrégateur ne peut pas tricher car il ne possède pas les clés privées des holders. Il ne peut pas forger leurs signatures. Son rôle est purement celui d'un coordinateur qui assemble des preuves cryptographiques produites par d'autres.

Si l'agrégateur est lent ou tombe en panne pendant la collecte, la transaction n'aboutit pas et l'utilisateur doit la resoumettre à un autre validator. Ce comportement simple est acceptable pour un MVP.

### Collecte des Attestations

Pour chaque objet standard déclaré dans la transaction, l'agrégateur contacte les N holders en parallèle, où N est le facteur de réplication de l'objet. Les singletons ne nécessitent pas de collecte d'attestations car tous les validators les possèdent déjà et peuvent vérifier leur version localement.

Chaque holder reçoit la requête contenant l'identifiant de l'objet et la version attendue, vérifie qu'il possède bien cet objet à cette version, et répond avec son attestation.

Le holder classé premier par Rendezvous Hashing sur l'identifiant de l'objet répond avec l'objet complet accompagné de sa signature BLS sur le hash de l'objet concaténé avec sa version. Les N-1 autres holders répondent uniquement avec le hash et leur signature BLS. Cette asymétrie réduit considérablement la bande passante : un seul holder envoie l'objet complet tandis que les autres n'envoient que ~150 bytes chacun (hash + signature BLS).

L'utilisation de signatures BLS permet l'agrégation : les signatures individuelles des holders sont combinées en une signature agrégée unique de 96 bytes, réduisant drastiquement la taille des preuves dans les vertices. Avec des implémentations modernes comme blst, la signature BLS prend environ 300μs, ce qui reste négligeable face aux latences réseau de 20-50ms.

### Validation du Quorum

L'agrégateur collecte les réponses des holders et vérifie que le quorum est atteint. Pour qu'une transaction soit validée, il faut que 67% des holders attendus signent le même hash pour chaque objet concerné.

L'agrégateur vérifie que le hash de l'objet complet reçu du holder top-1 correspond au hash majoritaire signé par les autres holders. Si un holder top-1 malicieux envoyait un faux objet, son hash ne correspondrait pas au hash attesté par les 49 autres holders honnêtes.

La liste des holders attendus est calculable par tous via le Rendezvous Hashing sur les objets déclarés dans la transaction. Tout validator peut vérifier indépendamment que la preuve de quorum est valide.

### Votes Négatifs et Fail-Fast

Si un holder ne possède pas l'objet demandé ou ne le possède pas à la version attendue, il répond avec un vote négatif explicite. Il signe un message de rejet spécifique indiquant la raison (objet inexistant, version incorrecte).

Dès que le nombre de votes négatifs rend le quorum mathématiquement impossible, l'agrégateur abandonne la collecte et rejette la transaction immédiatement sans attendre les réponses des autres holders. Par exemple, si une transaction implique un objet avec 50 holders et que 17 d'entre eux votent négatif, il ne reste plus assez de holders potentiels pour atteindre les 67% nécessaires. La transaction est donc rejetée en fail-fast.

Un validator qui ne détient aucun objet de la transaction n'est pas contacté par l'agrégateur. Seuls les holders sont sollicités.

### Agrégation BLS

Une fois le quorum atteint, l'agrégateur agrège les signatures BLS individuelles en une signature BLS agrégée unique accompagnée d'un bitmap indiquant quels holders ont signé.

Cette agrégation est une simple multiplication des signatures sur la courbe BLS12-381 et prend quelques microsecondes. La signature BLS agrégée est compacte : environ 96 bytes plus le bitmap, contre plusieurs KB si on stockait toutes les signatures individuellement.

Le vertex final contient la signature BLS agrégée. Tout validator peut vérifier cette signature agrégée pour confirmer que le quorum a bien été atteint.

### Sécurité des Objets Attestés

Une minorité malicieuse ne peut pas corrompre les objets attestés. Chaque holder possède l'objet localement et calcule son hash indépendamment. Si un holder malicieux envoie un faux objet ou un faux hash, son attestation sera différente de celle des holders honnêtes. Il se retrouvera isolé avec son propre hash que personne d'autre ne signe.

Les 34 holders honnêtes ou plus forment le quorum sur le vrai contenu de l'objet. Il faudrait 17 holders corrompus ou plus qui mentent avec exactement les mêmes fausses données pour tromper le système, mais à ce niveau ils contrôlent déjà le quorum et peuvent de toute façon faire ce qu'ils veulent.

Le versioning assure la cohérence temporelle. Si deux validators voient des versions différentes du même objet parce qu'une autre transaction l'a modifié entre-temps, leurs hashes seront différents et ne formeront pas un quorum cohérent. La transaction sera rejetée comme conflit de concurrence et devra être resoumise avec les bonnes versions.

### Détection des Comportements Malicieux

Si un holder signe un hash H mais envoie ensuite des données D incompatibles où hash(D) est différent de H, c'est prouvable on-chain. Le holder a signé H mais fourni des données incompatibles. Cette preuve de comportement malveillant entraîne un slash.

Si l'incohérence est due à une corruption réseau et non à une malveillance, l'agrégateur demande simplement les données à un autre holder ayant signé le même hash. Il n'y a pas de preuve de malveillance dans ce cas, donc pas de slash.

---

## Flow Complet d'une Transaction

### Étape 1 : Soumission

Un utilisateur envoie sa transaction à n'importe quel validator du réseau. La transaction contient le header avec les listes ReadObjects et MutableObjects, chaque objet étant identifié avec sa version attendue. Elle contient aussi le pod à appeler, la fonction, les arguments, et les signatures. Le validator qui reçoit la transaction devient l'agrégateur.

### Étape 2 : Identification des Holders

L'agrégateur calcule via Rendezvous Hashing la liste des holders pour chaque objet standard déclaré dans la transaction. Pour chaque objet, il identifie les N holders (selon le facteur de réplication de l'objet) et détermine le holder top-1 qui devra envoyer l'objet complet. Les singletons sont ignorés à cette étape car ils ne nécessitent pas de collecte d'attestations.

### Étape 3 : Collecte Parallèle des Attestations

L'agrégateur contacte tous les holders en parallèle via les connexions QUIC directes. Chaque holder reçoit une requête contenant la transaction.

### Étape 4 : Vérification et Réponse des Holders

Chaque holder qui reçoit la requête vérifie qu'il possède l'objet demandé à la version spécifiée dans la transaction.

Si l'objet existe à la bonne version, le holder calcule H = hash(objet + version). Le holder top-1 pour chaque objet répond avec l'objet complet et sa signature BLS sur H. Les autres holders répondent avec H et leur signature BLS sur H.

Si l'objet n'existe pas ou n'est pas à la bonne version, le holder répond avec un vote négatif signé.

### Étape 5 : Agrégation des Réponses

L'agrégateur collecte les réponses. Pour chaque objet, il vérifie que 67% des holders ont signé le même hash. Il vérifie également que le hash de l'objet complet reçu du holder top-1 correspond au hash majoritaire.

Si le quorum n'est pas atteint ou si les votes négatifs rendent le quorum impossible, la transaction est rejetée.

### Étape 6 : Agrégation BLS

Une fois le quorum atteint, l'agrégateur combine les signatures BLS individuelles en une signature BLS agrégée pour chaque objet. Cette opération est quasi-instantanée (multiplication sur la courbe).

### Étape 7 : Assemblage de la Transaction Finale

L'agrégateur assemble la transaction finale en y incluant les objets complets collectés auprès des holders top-1, accompagnés de leurs preuves de quorum (signature BLS agrégée + bitmap des signers).

### Étape 8 : Inclusion dans un Vertex

L'agrégateur inclut la transaction finale dans son prochain vertex. Un vertex peut contenir plusieurs transactions. Le vertex contient également les liens vers les vertices parents dans le DAG.

### Étape 9 : Gossip du Vertex

Le vertex est propagé via le gossip avec un fanout de 40. En 3 hops, le vertex atteint tout le réseau.

### Étape 10 : Validation du Vertex par le Réseau

Chaque validator qui reçoit le vertex vérifie sa validité. Pour chaque transaction incluse, il vérifie que la signature BLS agrégée est valide pour les holders indiqués dans le bitmap, et que ces holders représentent au moins 67% des holders attendus calculés via Rendezvous Hashing. Il vérifie également les liens parents et la signature du producteur.

### Étape 11 : Commit dans le DAG

Quand le vertex V est référencé par des vertices de 2 rounds ultérieurs produits par 67% des validators, V est committé. La transaction est finale et ordonnée de manière déterministe.

### Étape 12 : Vérification des Versions et Exécution

Après commit, chaque validator vérifie que les versions attendues par la transaction correspondent aux versions calculées depuis le DAG. Si une version ne correspond pas (conflit détecté), la transaction est marquée comme échouée et n'est pas exécutée.

Si les versions sont correctes, chaque holder d'au moins un objet dans MutableObjects appelle execute(tx). La transaction contient tous les objets attestés par le quorum. Chaque holder calcule le nouvel état des objets qu'il possède parmi ceux déclarés dans MutableObjects.

### Étape 13 : Stockage et Incrémentation des Versions

Chaque holder stocke le nouvel état de ses objets déclarés dans MutableObjects. La version de chaque objet mutable est incrémentée de 1, indépendamment du fait que le contenu ait effectivement changé ou non. Les objets dans ReadObjects conservent leur version inchangée.

### Étape 14 : Création de Nouveaux Objets

Si la transaction a le flag `creates_objects: true` dans son header, elle a été exécutée par tous les validators. L'identifiant de chaque nouvel objet est calculé de façon déterministe comme hash(tx_id + index_output). Chaque validator calcule s'il est holder du nouvel objet via Rendezvous Hashing et, si oui, stocke directement l'objet qu'il a lui-même calculé.

---

## Création de Nouveaux Objets

### Calcul Déterministe de l'Identifiant

Quand une transaction crée un nouvel objet, son identifiant est calculé de façon déterministe. L'identifiant est le hash de l'identifiant de la transaction combiné avec l'index de l'output dans la liste des objets créés. Cette méthode garantit l'unicité de l'identifiant et permet de connaître à l'avance les holders cibles via le Rendezvous Hashing. Une transaction peut créer au maximum 16 nouveaux objets.

### Exécution par Tous les Validators

Les transactions qui créent de nouveaux objets doivent déclarer le flag `creates_objects: true` dans leur header. Ce flag est explicitement défini par le client lors de la soumission de la transaction.

Lorsqu'une transaction porte ce flag, elle est exécutée par tous les validators du réseau, de la même manière que les transactions touchant uniquement des singletons. Cette exécution universelle permet à chaque validator de calculer de manière déterministe le contenu exact des objets créés.

### Stockage Direct par les Holders

Après l'exécution, chaque validator calcule via Rendezvous Hashing s'il est holder des nouveaux objets créés. Si oui, il stocke directement l'objet qu'il a lui-même calculé. Puisque l'exécution est déterministe, tous les validators calculent exactement le même contenu pour chaque nouvel objet.

Ce mécanisme élimine le besoin d'une transaction ObjectCreation séparée. La finalité est atteinte en 2 rounds au lieu de 4, car il n'y a pas de phase supplémentaire de propagation et validation des objets créés.

### Synchronisation des Nouveaux Holders

Quand un validator devient nouveau holder d'un objet suite à un changement d'epoch ou à la disparition d'un autre validator, il peut récupérer l'objet auprès des holders existants. Le DAG contient la trace de la transaction ayant créé l'objet, ce qui permet d'identifier le moment de création et de vérifier l'authenticité de l'objet reçu.

---

## Objectifs de Latence

L'architecture est conçue pour viser une finalité de l'ordre de 400 millisecondes. Ce budget se décompose théoriquement en plusieurs étapes sur le chemin critique :

- Réception de la transaction par l'agrégateur : dépend de la connexion de l'utilisateur
- Collecte parallèle des objets et attestations via QUIC direct : RTT vers les holders
- Agrégation BLS : quelques microsecondes
- Gossip du vertex sur 3 hops : dépend de la latence inter-validators
- Consensus DAG avec 2 rounds pour atteindre le commit

Ces estimations devront être validées par des benchmarks en conditions réelles. Les performances effectives dépendront de la qualité des connexions entre validators, de la charge du réseau, et de la distribution géographique des nœuds.

---

## Projections de Charge Réseau

Les estimations suivantes sont des projections théoriques basées sur l'architecture. Elles devront être validées par des mesures en conditions réelles.

### Taille des Transactions

Avec les nouvelles limites (4 KB par objet, 8 objets max par transaction), la taille des transactions varie selon leur complexité. Une transaction simple touchant 2 objets de 500 bytes chacun avec le header et les signatures pèse environ 1.5 KB. Une transaction complexe touchant 8 objets de 2 KB chacun peut atteindre 20 KB. En moyenne, on peut estimer une taille de transaction d'environ 1.5 KB pour des opérations courantes comme les transferts.

### Collecte des Attestations

Pour chaque objet standard d'une transaction, la collecte génère théoriquement le trafic suivant (exemple avec 50 holders). L'agrégateur envoie une requête à N holders, soit environ N × 50 bytes sortant. Le holder top-1 répond avec l'objet complet (moyenne ~500 bytes) plus signature BLS, soit environ 600 bytes. Les N-1 autres holders répondent avec hash (32 bytes) plus signature BLS (96 bytes), soit (N-1) × 128 bytes entrant. Avec N=50, le total par objet serait d'environ 7 KB entrant pour l'agrégateur. Les singletons ne génèrent pas de trafic de collecte.

### Gossip des Vertices

La charge principale provient du gossip des vertices contenant les transactions complètes avec leurs objets attestés et preuves BLS. À 25k TPS avec une taille moyenne de 1.5 KB par transaction, le débit de données uniques est de 37.5 MB/s. Avec le mécanisme de gossip (réception + forwarding), chaque validator traite environ 50 MB/s en réception et 70 MB/s en émission. Les vertices sont compressés avec zstd, réduisant la bande passante effective.

### Charge Totale par Validator

La charge totale dépend du TPS visé :

| TPS | Bande passante bidirectionnelle | Réseau requis |
|-----|--------------------------------|---------------|
| 1k | ~5 MB/s (~40 Mbps) | Fibre résidentielle |
| 5k | ~25 MB/s (~200 Mbps) | Fibre pro / VPS |
| 10k | ~50 MB/s (~400 Mbps) | Data center entrée de gamme |
| 25k | ~120 MB/s (~1 Gbps) | Data center standard |

Ces estimations montrent que le réseau scale linéairement avec le TPS. Un objectif initial de 1k TPS est atteignable avec une infrastructure modeste, tandis que 25k TPS nécessite des validators en data center avec des connexions 10 Gbps.

---

## Incentives et Sécurité

### Vérification du Stockage

Le Rendezvous Hashing permet de savoir exactement qui doit stocker quoi. Les validators peuvent être challengés pour prouver qu'ils détiennent bien les objets dont ils sont censés être holders. Un validator qui échoue à répondre à un challenge de preuve de stockage subit un slash sur son stake.

### Détection des Votes Malicieux

Les votes malicieux sont détectables. Un validator qui vote négativement sur une transaction valide ou qui atteste de fausses valeurs produit un hash différent de celui des holders honnêtes. Son vote se retrouve isolé et ne contribue pas au quorum des votes honnêtes.

Si un holder signe un hash H mais envoie ensuite des données D incompatibles où hash(D) est différent de H, c'est prouvable on-chain. Cette preuve entraîne un slash.

---

## Problèmes Ouverts

### Fraud Proofs pour Mauvaise Exécution

Le mécanisme exact de fraud proof si un holder exécute mal une transaction et produit un mauvais état n'est pas encore défini. Plusieurs holders exécutent la même transaction et devraient produire le même résultat pour les objets qu'ils partagent. Un mécanisme de détection et punition des exécutions incorrectes reste à concevoir.

### Système de Challenge pour le Stockage

Les détails du système de challenge et proof pour le stockage restent à affiner. Cela inclut le format exact de la preuve de possession d'un objet, le coût du challenge pour éviter le spam, et la fréquence des challenges par validator.

### Tolérance aux Pannes de l'Agrégateur

Actuellement, si l'agrégateur tombe en panne pendant la collecte des attestations, la transaction n'aboutit pas et l'utilisateur doit la resoumettre à un autre validator. Un mécanisme de failover automatique où un autre validator reprend le rôle d'agrégateur pourrait être envisagé dans une version future.
