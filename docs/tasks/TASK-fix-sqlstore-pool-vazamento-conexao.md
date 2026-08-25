# Corrigir: `StartClient()` cria uma pool Postgres nova a cada tentativa e nunca fecha — esgota `max_connections`

## Sintoma em produção (achado em 25/08/2026, via diagnóstico do ERP Aviv)

Ao tentar parear uma instância NOVA (`POST /instance/connect` → `GET
/instance/qr` em loop, como o front do ERP faz), o QR nunca aparecia. `GET
/instance/qr` sempre devolvia 400 depois de ~5s:

```
{"error":"no QR code available. Please wait a moment and try again"}
```

Os logs do container `api` mostravam, repetindo a cada ~7s indefinidamente:

```
[INFO] Starting websocket connection to Whatsapp for user '<id>'
[INFO] [<id>] Waiting for QR code generation...
[ERR]  [<id>] Failed to create container: failed to upgrade database: failed to check if version table is up to date: pq: sorry, too many clients already
[INFO] [<id>] No QR code available yet, waiting a bit more...
[INFO] [<id>] No client found, starting new instance for QR code
```

E o container `postgres` (o Postgres PRÓPRIO deste projeto — não o do ERP)
mostrava, também repetindo:

```
FATAL:  sorry, too many clients already
```

O gateway nunca conseguia sequer abrir o `sqlstore.Container` (device store
do whatsmeow) — então o cliente WhatsApp nunca chegava a existir, nunca
conectava, e nunca emitia QR nenhum. Não é sintoma de "QR demorando" — é o
loop de retry batendo, a cada tentativa, no teto de conexões do Postgres.

## Causa raiz (leitura de código — não confirmada com repro isolado local,
## `go build` não disponível neste ambiente; ver "Como confirmar" abaixo)

`pkg/whatsmeow/service/whatsmeow.go`, dentro de `StartClient()` (linhas
356-373):

```go
var container *sqlstore.Container

if w.config.WaDebug != "" {
    dbLog := waLog.Stdout("Database", w.config.WaDebug, true)
    if w.config.PostgresAuthDB != "" {
        container, err = sqlstore.New(context.Background(), "postgres", w.config.PostgresAuthDB, dbLog)
    } else {
        ...sqlite...
    }
} else {
    if w.config.PostgresAuthDB != "" {
        container, err = sqlstore.New(context.Background(), "postgres", w.config.PostgresAuthDB, nil)
    } else {
        ...sqlite...
    }
}
```

Dois problemas compostos:

1. **`sqlstore.New(ctx, "postgres", dsn, log)` recebe a CONNECTION STRING
   crua**, não um `*sql.DB` existente. Internamente a lib whatsmeow faz seu
   próprio `sql.Open("postgres", dsn)` — ou seja, **uma pool Postgres nova a
   cada chamada de `StartClient()`**. Essa pool não tem `SetMaxOpenConns`
   configurado (default do Go: ilimitado).

2. **`container` é uma variável LOCAL da função — nunca é fechado.** Não há
   `container.Close()` em `StartClient()`, nem no branch de erro (linha
   375-378), nem em nenhum outro caminho de saída da função, nem no teardown
   de `ReconnectClient()` (linhas 174-254, que também nunca fecha nada). A
   conexão fica pendurada no servidor Postgres até o processo do gateway
   reiniciar — GC do Go não fecha sockets de rede/DB.

O irônico: o processo **já** cria e mantém um `*sql.DB` corretamente
configurado para essa mesma DSN — `w.authDB`, criado uma única vez em
`cmd/evolution-go/main.go` (`initPostgresAuthDB`, ~linha 300-327):

```go
db.SetMaxOpenConns(25)
db.SetMaxIdleConns(5)
db.SetConnMaxLifetime(5 * time.Minute)
db.SetConnMaxIdleTime(1 * time.Minute)
```

Esse `w.authDB` está disponível no `whatsmeowService` (campo `authDB
*sql.DB`, linha ~81) mas hoje só é usado para uma query avulsa em
`ForceUpdateJid` (linha ~265) — nunca é reaproveitado pelo `sqlstore.New`,
apesar da lib whatsmeow ter `sqlstore.NewWithDB(db *sql.DB, dialect string,
log waLog.Logger) *Container` feito exatamente para este caso (reusar um
pool já pronto/limitado em vez de a lib abrir o dela).

**Cada instância gerenciada** (não só durante pareamento QR — todo
`StartClient`/reconexão automática pós-desconexão) abre sua própria pool
isolada e sem limite. Com múltiplas instâncias reais ativas + qualquer ciclo
de reconexão/retry, o total de conexões abertas ao mesmo tempo cresce sem
teto até estourar o `max_connections` do Postgres (default 100 — não há
override customizado em nenhum dos `docker-compose*.yml` de exemplo do
repo). **Subir o `max_connections` do Postgres adiaria o sintoma, não
elimina a causa** — o vazamento por-tentativa continua idêntico.

Relação com o commit `d04823b` ("serializar StartClient por instância para
fechar race no pareamento"): esse commit resolveu uma race DIFERENTE (dois
`StartClient` concorrentes para a mesma instância disputando a mesma linha
`instance.qrcode`). Ele não mexe em `sqlstore.New` nem adiciona nenhum
`Close()`. O mutex serializa chamadas *simultâneas*; o loop de `GetQr()`
gera chamadas *sequenciais* repetidas (a cada ~7s), e cada uma continua
abrindo uma pool nova e nunca fechada — o vazamento é ortogonal a esse fix e
já existia antes dele.

## O que fazer

1. **Trocar as 4 chamadas `sqlstore.New(context.Background(), "postgres",
   w.config.PostgresAuthDB, <log>)` por `sqlstore.NewWithDB(w.authDB,
   "postgres", <log>)`** (linhas 361 e 368 de
   `pkg/whatsmeow/service/whatsmeow.go`) — reaproveita o pool já existente,
   já limitado (`SetMaxOpenConns(25)`), compartilhado entre todas as
   instâncias. Os 2 branches de sqlite (linhas 364, 371) não são afetados
   (sqlite não tem esse problema de pool-por-tentativa da mesma forma, e já
   usa uma DSN de arquivo compartilhada por instância).
2. **Confirmar que `sqlstore.NewWithDB` existe na versão vendorizada de
   `go.mau.fi/whatsmeow` deste repo** (`go.mod`:
   `go.mau.fi/whatsmeow v0.0.0-20260630180629-b572e5bcb92b`) antes de
   considerar a correção fechada — não há toolchain Go neste ambiente pra
   confirmar com `go build`/`go doc` localmente; a CI (`publish_ghcr.yml`,
   dispara em qualquer push pra `fix/**`) é quem vai realmente compilar.
3. Considerar (fora do escopo mínimo desta correção, mas mesma causa raiz):
   nenhum outro caminho do arquivo fecha `container.Close()` — se
   `NewWithDB` também devolver algo que precise de `Close()` explícito em
   algum fluxo de teardown (desconexão definitiva de instância), vale
   auditar `ReconnectClient()` e o branch do kill-channel (linhas ~616-673)
   na mesma revisão.

## Onde olhar

- `pkg/whatsmeow/service/whatsmeow.go`:
  - `StartClient()` (linhas ~336-450), especificamente 356-373
  - campo `authDB *sql.DB` (~linha 81) e seu único uso atual (~linha 265,
    `ForceUpdateJid`)
  - `ReconnectClient()` (linhas ~174-254) — mesma ausência de `Close()`
- `cmd/evolution-go/main.go`: `initPostgresAuthDB` (~linha 300-327) — onde
  `w.authDB` é criado e configurado; e onde ele é passado pro
  `whatsmeowService` (~linha 2864/2884 do arquivo do service)
- `pkg/instance/service/instance_service.go`, `GetQr()` (~linha 419-472) —
  o loop que chama `StartInstance()`/`StartClient()` a cada ~7s enquanto o
  QR não existe (3s + 2s de `time.Sleep` fixos, mais overhead)

## Como confirmar que corrigiu

Não reproduzido isolado localmente (exigiria um Postgres com
`max_connections` baixo de propósito + go toolchain, nenhum dos dois
disponível neste ambiente de diagnóstico). Depois do fix, validar:

- **Na CI**: o build (`publish_ghcr.yml`) precisa passar — é a única
  compilação real disponível até agora.
- **Em produção, após deploy da imagem corrigida**: abrir/parear várias
  instâncias em sequência (ou reproduzir o mesmo loop de retry de propósito
  deixando uma instância sem escanear por alguns minutos) e confirmar via
  `SELECT count(*) FROM pg_stat_activity;` no Postgres do gateway que o
  número de conexões **não cresce sem limite** — deve estabilizar perto do
  `SetMaxOpenConns(25)` do `w.authDB`, não continuar subindo a cada
  tentativa.
- Confirmar que o QR passa a aparecer normalmente numa instância nova (sem
  o loop `Failed to create container: ... too many clients already`).
