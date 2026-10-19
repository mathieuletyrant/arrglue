# arrglue — spec de conception

Oct 19, 2026 · Mathieu Le Tyrant

## Contexte et objectifs

arrglue est un outil Go qui découvre les apps de la stack *arr où qu'elles tournent, calcule comment chacune peut joindre les autres, et applique les connexions via leurs API, de façon idempotente.

Le problème : chaque app est simple, mais les relier demande de connaître, pour chaque couple, la bonne URL depuis le point de vue de l'appelant, la clé API de l'appelé et la correspondance des chemins de fichiers. Ces trois informations changent selon que l'app tourne en Docker, en natif ou sur une plateforme comme Umbrel.

### Objectifs
- Inventorier automatiquement les apps (type, version, réseau, montages, clé).
- Résoudre une URL vérifiée par couple appelant → appelé.
- Détecter les problèmes de chemins et de hardlinks avant qu'ils ne coûtent une copie de 50 Go.
- Créer et maintenir les connexions (plan / apply), et recâbler en cas de dérive.
- Exposer un registre que d'autres outils interrogent au lieu de demander URL et clés à l'utilisateur.

### Non-objectifs
- Gérer les profils qualité et formats personnalisés : c'est le rôle de Recyclarr, Configarr ou Profilarr, qui pourront consommer le registre.
- Installer ou mettre à jour les apps.
- Remplacer l'UI des apps ou leur authentification.

### Périmètre initial (setup cible)
Umbrel : radarr, sonarr, prowlarr, transmission, jellyfin, jellyseerr.

## Architecture

Quatre étapes en chaîne, pilotées par un catalogue commun : découvrir, modéliser, résoudre, appliquer.
Les fournisseurs ne font que lire les apps ; seuls les connecteurs écrivent, et uniquement via les API, après un plan approuvé.

### Catalogue d'apps

Dossier de fichiers YAML, un par type d'app, embarqué dans le binaire (embed.FS) et surchargeable par un dossier local. Ajouter une app ne demande pas d'écrire du Go.

```yaml
# catalog/radarr.yaml
kind: radarr
family: servarr
detect:
  images: ["*/radarr", "linuxserver/radarr", "hotio/radarr"]
  binaries: [Radarr]
  systemd: [radarr.service]
ports: { http: 7878 }
config:
  paths: [/config/config.xml, ~/.config/Radarr/config.xml, /var/lib/radarr/config.xml]
  format: xml
  fields:
    api_key: /Config/ApiKey
    port: /Config/Port
    url_base: /Config/UrlBase
inject:
  env:
    api_key: RADARR__AUTH__APIKEY
health:
  path: /api/v3/system/status
  auth: { header: X-Api-Key }
  version_field: version
api:
  versions: [v3]
```

| Champ | Rôle |
|---|---|
| detect | Motifs d'image, nom de binaire, unité systemd pour reconnaître l'app |
| ports | Ports internes par défaut, utilisés si la config ne les donne pas |
| config | Où lire la config et quels champs en extraire (XML, YAML, JSON, INI) |
| inject | Comment imposer une clé à l'installation (variable d'environnement), si l'app le permet |
| health | Endpoint pour vérifier qu'une URL et une clé fonctionnent |
| api.versions | Versions d'API supportées, pour choisir les variantes de connecteurs |

Types à couvrir à terme : Radarr, Sonarr, Lidarr, Readarr, Prowlarr (famille Servarr), Bazarr, Transmission, qBittorrent, SABnzbd, Plex, Jellyfin, Seerr. Pour Transmission (settings.json, identifiants RPC) ou Plex (Preferences.xml, PlexOnlineToken), seuls le format et les champs changent.

### Fournisseurs de découverte

Chaque fournisseur parcourt une plateforme et produit des Instance normalisées ; le reste du moteur ignore d'où elles viennent.

```go
type Provider interface {
    Name() string
    Discover(ctx context.Context, cat *catalog.Catalog) ([]Instance, error)
    Watch(ctx context.Context) (<-chan Event, error) // optionnel : nil si pas d'événements
}

type Instance struct {
    ID        string            // stable : "docker:radarr_server_1", "systemd:sonarr"
    Kind      string            // "radarr", issu du catalogue
    Version   string
    Provider  string
    Netns     NetNamespace      // réseau(x) Docker, host, ou natif
    Endpoints []Endpoint        // port interne, port publié, nom DNS
    Mounts    []Mount           // chemin vu par l'app -> chemin hôte
    Secret    SecretRef         // où la clé a été trouvée, jamais la clé en clair
    Labels    map[string]string
}
```

| Fournisseur | Sources | Lecture de la config |
|---|---|---|
| Docker | Socket Docker : conteneurs, images, labels, réseaux, montages, ports publiés, docker events | Via le chemin hôte du montage /config |
| Natif | Unités systemd, /proc/*/cmdline, ports en écoute, dossiers de config standards | Directement sur le disque |
| Umbrel | Fournisseur Docker plus les conventions Umbrel (voir Contraintes Umbrel) | Via app-data monté en lecture seule |
| Manuel | Fichier instances.yaml : apps distantes, seedbox | Clé fournie par l'utilisateur |
| Kubernetes (plus tard) | client-go : pods, services, PVC | Via les secrets ou volumes |

Règles de détection :
- Un conteneur est rattaché à un type par motif d'image, puis par label `arrglue.kind` qui prime sur tout.
- Un label `arrglue.ignore=true` exclut une instance.
- En cas de doublon (même app vue par deux fournisseurs), l'instance la plus précise gagne : Umbrel avant Docker, Docker avant natif.

### Résolution d'accessibilité

Une URL n'est valable que pour un appelant donné : arrglue calcule et vérifie une URL par couple appelant → appelé, depuis le réseau de l'appelant.

Génération des candidates, par ordre de préférence :
1. Réseau Docker commun : `http://<nom_conteneur>:<port_interne><url_base>`. Les alias réseau sont aussi essayés.
2. Appelant en mode host ou natif, appelé publié : `http://127.0.0.1:<port_publié>`, puis l'IP LAN de l'hôte.
3. Conteneur vers natif ou vers port publié : passerelle du bridge de l'appelant, ou `host.docker.internal` s'il est défini.
4. URL manuelle déclarée dans instances.yaml.

Les URL qui passent par un reverse proxy authentifié ou par un tunnel public ne sont jamais proposées pour un lien entre apps.

Vérification par sonde. Pour un appelant en conteneur, arrglue lance un conteneur éphémère avec `--network container:<appelant>` : il partage exactement la pile réseau de l'appelant (DNS, routes, interfaces). La sonde appelle l'endpoint health du catalogue avec la clé et renvoie code HTTP, latence et version. Pour un appelant natif, la sonde est un simple appel HTTP depuis l'hôte.
- Image de sonde minimale (binaire `arrglue probe` dans une image scratch), supprimée après usage.
- Résultats mis en cache par couple, invalidés par un événement Docker touchant l'un des deux.
- Mode dégradé sans création de conteneur : on se contente du calcul théorique, marqué « non vérifié ».

Résultat : une table `Reachability{From, To, URL, Verified, Error}` utilisée par les connecteurs et exposée par doctor.

### Résolution des chemins et hardlinks

Comme arrglue connaît les montages de chaque instance, il traduit tout chemin vu par une app en chemin hôte, puis en chemin vu par une autre app.
- `ToHost(inst, p)` : chemin vu par l'app → chemin hôte, via le montage au préfixe le plus long.
- `FromHost(inst, h)` : chemin hôte → chemin vu par l'app, ou erreur si l'app ne voit pas ce dossier.
- `Translate(a, b, p) = FromHost(b, ToHost(a, p))`.

| Cas | Ce que fait arrglue |
|---|---|
| Dossier de téléchargement du client invisible pour Radarr | doctor signale l'erreur avec le chemin manquant ; plan ne crée pas le lien |
| Même dossier, chemins différents | Génère le remote path mapping dans Radarr/Sonarr |
| Dossier de téléchargement et dossier racine sur des systèmes de fichiers différents | Avertit que les hardlinks sont impossibles (comparaison du st_dev côté hôte) |
| Montages séparés /downloads et /movies du même disque | Avertit : même disque mais deux montages, donc pas de hardlink dans le conteneur ; suggère un montage parent unique |

Les chemins des dossiers racine et du client de téléchargement sont lus via les API, pas déduits ; arrglue ne fait que vérifier et traduire.

### Connecteurs

Un connecteur sait créer et maintenir un type de lien entre deux apps ; il ne touche qu'aux objets qu'il a créés.

```go
type Connector interface {
    ID() string                                   // "prowlarr->servarr"
    Matches(from, to Instance) bool
    Read(ctx context.Context, l Link) (State, error)    // état actuel via l'API
    Desired(g *Graph, l Link) (State, error)            // URL, clé, chemins, options
    Diff(cur, want State) []Change
    Apply(ctx context.Context, l Link, ch []Change) error
}
```

Propriété. Tout objet créé porte un marqueur : un tag `arrglue` quand l'API gère les tags, sinon un nom préfixé `[arrglue]`. Un objet équivalent créé à la main est signalé, jamais modifié, sauf option `--adopt`.

| Connecteur | Objet créé | Points d'attention |
|---|---|---|
| Prowlarr → Radarr/Sonarr/Lidarr | Application dans Prowlarr | Deux URL : celle de l'app vue par Prowlarr, et celle de Prowlarr vue par l'app |
| Radarr/Sonarr → client de téléchargement | Download client + remote path mapping | Catégorie par app, traduction des chemins |
| Bazarr → Radarr/Sonarr | Réglages Radarr/Sonarr de Bazarr | Correspondance des chemins médias |
| Seerr → Plex/Jellyfin, Radarr/Sonarr | Serveurs dans Seerr | Profil et dossier racine par défaut à choisir |
| Serveur média ← Radarr/Sonarr | Notification « rafraîchir la bibliothèque » | Jeton Plex ou clé Jellyfin |

Un connecteur simple peut être décrit en YAML (endpoint, mapping de champs, clé d'identité pour le diff) ; le code Go reste pour les cas à logique.

## Moteur, démon et interfaces

Le moteur suit le cycle scan → doctor → plan → apply, en simulation par défaut ; le démon répète ce cycle sur événement et à intervalle fixe.

État désiré (`arrglue.yaml`) :
```yaml
links:
  - from: prowlarr
    to: [radarr, sonarr]
  - from: [radarr, sonarr]
    to: transmission
    options: { category_from_kind: true }
  - from: bazarr
    to: [radarr, sonarr]
auto: true        # propose aussi les liens évidents non déclarés
adopt: false
```

| Commande | Effet |
|---|---|
| arrglue scan | Affiche les instances trouvées, sans rien écrire |
| arrglue doctor | Liens manquants, cassés ou dangereux (URL injoignable, hardlink impossible, clé exposée) |
| arrglue plan | Liste les changements prévus, à la Terraform |
| arrglue apply | Applique le plan après confirmation (`--yes` pour l'automatiser) |
| arrglue serve | Démon : réconciliation, UI web, API de registre |
| arrglue probe | Sous-commande interne exécutée dans le conteneur sonde |

Démon :
- Déclencheurs : docker events (start, die, network connect), intervalle (15 min par défaut), appel API.
- En mode serve, apply automatique désactivé par défaut ; activable lien par lien.
- État et historique dans SQLite : instances, résultats de sonde, plans appliqués.

Interfaces exposées :
- UI web minimale : graphe des instances et état de chaque lien.
- API de registre `GET /v1/services` et `GET /v1/services/{id}?for=<appelant>` : renvoie l'URL résolue pour cet appelant et, avec un jeton autorisé, la clé.
- Serveur MCP optionnel : scan, doctor, plan, apply comme outils.

## Contraintes Umbrel et packaging

Sur Umbrel, arrglue est une app de store communautaire qui découvre les clés et passe par les API ; elle n'injecte rien dans les autres apps.

| Contrainte Umbrel | Conséquence pour arrglue |
|---|---|
| exports.sh n'est chargé que pour les dépendances déclarées | Pas de câblage par variables entre apps ; tout passe par arrglue, aucune app ne dépend de lui |
| Les montages ajoutés par l'utilisateur refusent /Apps | Le docker-compose.yml de l'app monte lui-même `${APP_DATA_DIR}/..` (racine app-data) en lecture seule |
| app_proxy impose l'authentification Umbrel | Appels directs à `<app-id>_<service>_1` sur `umbrel_main_network`, jamais via le port publié |
| Le compose des autres apps n'est pas modifiable | Pas d'injection de clé par variable : lecture des clés existantes uniquement |
| Une mise à jour recrée les conteneurs | Noms stables, IP non : on ne stocke que des noms |

Accès au socket Docker : derrière un proxy de socket qui n'autorise que les lectures (containers, networks, events). Le `:ro` sur le socket ne restreint pas l'API. La création du conteneur sonde nécessite en plus `POST /containers` ; si on préfère s'en passer, la vérification se fait depuis le conteneur arrglue lui-même, qui est sur `umbrel_main_network` comme les apps.

Packaging général :
- Binaire statique Go (CGO_ENABLED=0), linux/amd64 et linux/arm64, plus macOS pour la CLI.
- Image FROM scratch multi-arch avec le catalogue embarqué.
- Exemple docker-compose.yml générique : socket via proxy, dossiers de config des apps montés en lecture seule.
- Paquet systemd pour les installations natives.

## Sécurité, structure du repo et roadmap

Sécurité. arrglue concentre toutes les clés de la stack : il reste interne et n'écrit que sur demande.
- Clés chiffrées au repos dans SQLite (clé maître en variable d'environnement ou fichier 0600), jamais dans les logs.
- API de registre sur le réseau interne uniquement, avec jeton ; la clé n'est renvoyée qu'aux jetons qui y ont droit.
- Lectures de config en lecture seule ; écritures uniquement via les API des apps, et uniquement par apply.
- Aucune route publique recommandée (pas de tunnel, pas de reverse proxy exposé).

Structure du repo Go :
```
arrglue/
  cmd/arrglue/          # main, commandes
  internal/catalog/     # chargement et validation des YAML
  internal/provider/    # docker, native, umbrel, manual
  internal/graph/       # Instance, Link, Graph
  internal/reach/       # candidates d'URL, sonde
  internal/paths/       # traduction des chemins, hardlinks
  internal/connector/   # interface + connecteurs
  internal/engine/      # plan, apply, démon
  internal/store/       # SQLite, chiffrement des secrets
  internal/api/         # registre HTTP, UI web
  internal/mcp/         # serveur MCP optionnel
  catalog/              # YAML embarqués (embed.FS)
  deploy/               # compose générique, unité systemd, app Umbrel
```

Dépendances envisagées : client Docker officiel (github.com/docker/docker/client), gopkg.in/yaml.v3, encoding/xml, gopkg.in/ini.v1, spf13/cobra, modernc.org/sqlite (sans CGO), SDK Go officiel MCP pour la partie MCP.

Roadmap :
1. Catalogue + fournisseur Docker + scan et doctor en lecture seule.
2. Sonde réseau et résolution des chemins.
3. Connecteurs Prowlarr → apps, apps → client de téléchargement ; plan et apply.
4. Fournisseurs Umbrel, natif et manuel ; démon, registre, UI web.
5. Seerr, serveurs média, connecteurs YAML, serveur MCP, Kubernetes.

Questions ouvertes :
- Faut-il un mode `--adopt` par défaut pour les installs existantes, ou toujours explicite ?
- Sonde par conteneur éphémère sur Umbrel : acceptable, ou vérification depuis le conteneur arrglue seulement ?
- Nom définitif du projet.
