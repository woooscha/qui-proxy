# qui–qBittorrent-proxy: isolerad PoC

Detta är ett **overifierat proof of concept**, inte en produktionsklar proxy. Det ska endast användas i en separat qui-testinstans med automatiska sökningar och andra automatiska ändringar avstängda. Ingen anslutning har gjorts till användarens Unraid-installation och inget verkligt reflink- eller seedningstest har utförts. Kompilering och tester måste lyckas innan containern används; se verifieringsstatus nedan.

PoC:n är avgränsad till en angiven infohash och en uttryckligen bind-monterad XFS-datakatalog. qBittorrent 1 behöver inte ändras. Endast login och en liten uttrycklig lista över GET-anrop tillåts. Övriga anrop blockeras, även GET-anrop som kan ändra data. Proxyn skapar inte reflinks eller filer. qBittorrent-sessionscookies vidarebefordras.

För det angivna hashvärdet hämtar proxyn aktuell torrentinformation och fillista med samma sessionscookie inför svar för `torrents/info`, `torrents/properties` och `sync/maindata`. Hela listan måste vara komplett, och varje fil måste vara en vanlig fil med förväntad storlek på samma XFS-filsystem. Inga symlänkar i filsökvägarna tillåts. Den konfigurerade datakatalogen måste vara en egen skrivskyddad XFS-monteringspunkt i proxyns mount namespace. Fel ger HTTP 502 och ingen omskriven respons. `save_path`, `content_path` och `download_path` översätts när de förekommer och kan verifieras; tomma värden lämnas tomma. Relativa filnamn ändras inte. RID och övriga sync-fält vidarebefordras; ändringar för andra torrents ändras inte.

## Viktiga begränsningar

- Denna PoC undersöker enbart disk4. Den kan inte upptäcka kopior på andra diskar eller avgöra om en fil med samma namn och storlek har rätt innehåll. qBittorrent 2 måste verifiera torrentinnehållet. Automatisk diskupptäckt, kontroll av tvetydighet över diskar och cache ingår inte ännu.
- Ett misslyckat API-svar kan inte dra tillbaka en sökväg som qui redan har cachat. Det finns också ett tidsfönster mellan filkontroll och quis reflink-anrop. Håll testet manuellt och isolerat och avbryt det om disken försvinner eller filer flyttas. Fail-closed genom hela qui-flödet är **inte verifierat**.
- Endast `/api/v2/auth/login`, GET `/api/v2/app/version`, `/api/v2/app/webapiVersion`, `/api/v2/torrents/info`, `/api/v2/torrents/files`, `/api/v2/torrents/properties` och `/api/v2/sync/maindata` är tillåtna. `files` och `properties` kräver den konfigurerade hashen. Det är ännu inte bekräftat att detta räcker för den installerade qui-versionen; utöka inte listan utan att kontrollera varje ändpunkts beteende.
- Endast en torrent får översatta sökvägar; andra torrenters data i list- och synksvar förblir oförändrade. Kör aldrig automatiska cross-seed-jobb mot denna PoC.
- Storlekskontroller kräver att hela torrenten finns på disk4, med färdigställda filer och oförändrade qBittorrent-filnamn. Incomplete-torrents, delvis valda torrents, symlänkar och andra oklarheter avvisas.
- Healthcheck är en liveness-kontroll av HTTP-processen, inte en kontroll av qBittorrent, diskens innehåll eller qui.
- Varje läsanrop som översätts gör extra API- och filsystemsanrop. Det finns ingen cache. Samtidiga ändringar kan leda till att svaret avvisas.
- Sökvägsfältens betydelse och quis användning av dem, inklusive tomma eller separata `download_path`, måste verifieras mot de faktiska versionerna. En sökväg utanför `/data` eller en icke-existerande `content_path`/`download_path` avvisas.

## Förbered testet på Unraid

1. Kontrollera att disk4 verkligen är monterad som XFS och att testtorrentens samtliga filer ligger där. Välj den färdiga torrenten `Deadliest.Catch.S22E19.1080p.WEB.h264-EDITH` och ta fram dess infohash i qBittorrent 1. Filen måste synas under `/mnt/disk4/data/torrents/tv/...` och via `/data/torrents/tv/...` i qBittorrent 1. Låt qBittorrent 1:s konfiguration vara oförändrad.
2. Kopiera `config.example.json` till `config.json`; sätt den faktiska hashen och kontrollera upstreamadressen och båda sökvägsprefixen. Skapa en isolerad Docker-nätverksanslutning så att proxyn kan nå qBittorrent 1 och test-qui kan nå proxyn. Exemplet använder ett redan existerande externt nätverk `qui-poc`; anpassa nätverket efter installationen. Publicera ingen proxyport till värden eller internet.
3. Bygg och starta `docker compose up -d --build` först efter att bygg- och testkontrollerna lyckats. Proxyn kör som icke-root och behöver läs- och sökbehörighet till samtliga källfiler. Bind-monteringen måste vara exakt `/mnt/disk4/data` i containern och skrivskyddad. Verifiera faktisk `mountinfo`, XFS och läsbehörigheter i installationen; en vanlig katalog på ett annat filsystem godtas inte.
4. Använd en separat qui-instans med automatiska jobb avstängda och med samma källsökväg `/mnt/disk4/data` monterad läsbart. Skapa en separat destination `/mnt/disk4/data/cross-seed-poc` på samma XFS-filsystem med skrivåtkomst för qui. Ge qBittorrent 2 åtkomst till destinationen och kontrollera dess faktiska containersökväg. Den ska motsvara quis route-mappning. Skydda originaldata genom separata läs- och skrivmonteringar och kontrollera att behörigheter fungerar.
5. Konfigurera quis Base directory och Cross-seed instance routing för disk4 och qBittorrent 2. Dokumentera installerad qui-version, dess inställningar och kontrollera relevant källkod/dokumentation för just den versionen. Exempelprojektet har ingen verifierad automatisk uppdatering av Base directories.

## Manuell verifiering

- Autentisera genom proxyn från test-qui och kontrollera att sessionscookie fungerar. Jämför den valda torrentens API-data före och efter proxyn. Kontrollera JSON, `save_path` och eventuella `content_path`/`download_path`, samt ett fullt och flera inkrementella sync-svar med RID. Kontrollera att andra torrenters uppgifter inte ändrats.
- Kontrollera från quis container att den översatta sökvägen kan läsas och att källa och destination verkligen ligger på samma XFS-filsystem. Prova negativt med en saknad fil, fel storlek och en otillgänglig disk i en isolerad testmiljö. Kontrollera att proxyn ger fel och att inga automatiska jobb fortsätter med tidigare cachade uppgifter.
- Starta **en** manuell cross-seed för testtorrenten när en matchning finns. Kontrollera i quis logg att reflink valts utan fallback till kopiering, att qBittorrent 2 får rätt sökväg och att dess verifiering lyckas innan seedning. Verifiera reflink/CoW med lämpligt XFS-verktyg och ett säkert separat test av klonens ändringar. Kontrollera att originalets innehåll inte ändrats. Dokumentera resultat och versioner.
- Kontrollera att ändrande API-anrop mot proxyadressen returnerar 403 och att ingen ändring skett i qBittorrent 1. Om qui kräver fler anrop eller skickar omskrivna sökvägar tillbaka till källinstansen, stoppa testet och utred först.

Dessa är testinstruktioner, inte bevis på att testen har genomförts.

## Nästa fas: diskar och drift

Produktionsfasen måste upptäcka alla tillgängliga XFS-diskar dynamiskt, inklusive disk10 och disk20, och avvisa tvetydiga samt spridda torrents. Diskupptäckt ska vara separat från en säkert invaliderad torrentcache. Testa avmontering, återanslutning och nya diskar, inklusive tidsfönstret efter att qui fått en sökväg. Ett fullständigt test på en ny fysisk disk krävs.

Separata bind mounts per disk begränsar åtkomst men kräver ändringar av Docker-konfigurationen vid expansion. En överordnad `/mnt`-mount med propagation kan synliggöra nya diskar men ger bredare åtkomst och måste utredas och verifieras. Även qui och qBittorrent 2 måste få rätt käll- och destinationsmonteringar, och quis Base directories/routes kan behöva läggas till manuellt för varje ny disk. Inga ändringar av qBittorrent 1 ska behövas. Anta inte att mount propagation eller automatisk Base directory-konfiguration fungerar innan det testats på installationen.

## Utveckling och verifieringsstatus

Avsett för Linux och Go 1.26+. Kör `go mod tidy`, `go fmt ./...`, `go fix ./...`, `go vet ./...`, `golangci-lint run ./...` och `go test -race ./...` i den ordningen före commit. Testerna använder temporära kataloger och en simulerad qBittorrent-server; de ersätter inte test av en riktig XFS-mount eller qui.

I denna arbetsmiljö saknas Go-kompilator och golangci-lint; miljöns nätverksproxy kunde inte nås för att installera Go. Docker-klienten saknar åtkomst till Docker-daemonen. Därför har koden, testerna och Docker-imagen **inte kunnat byggas eller köras**. Verifiera dem innan användning. Inga autentiseringsuppgifter eller anslutningar till användarens Unraid-miljö fanns tillgängliga.
