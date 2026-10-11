# api-engine

Modulo Go `github.com/karavy/api-engine` per eseguire chiamate API **GET, PUT, POST e DELETE**. Pensato per essere incorporato in un operator Kubernetes, senza dipendenze da Kubernetes o librerie esterne.

## Struttura

- [go.mod](go.mod): definizione del modulo.
- [pkg/apiengine/client.go](pkg/apiengine/client.go): package pubblico del client HTTP.
- [pkg/apiengine/retry.go](pkg/apiengine/retry.go): policy dei retry e backoff.
- [pkg/apiengine/client_test.go](pkg/apiengine/client_test.go): test unitari, mantenuti accanto al codice secondo le convenzioni Go.
- [pkg/apiengine/retry_test.go](pkg/apiengine/retry_test.go): test di retry, idempotenza, cancellazione e concorrenza.
- [pkg/apiengine/doc_test.go](pkg/apiengine/doc_test.go): esempio eseguibile dell'API pubblica.

Il modulo resta `github.com/karavy/api-engine`; il package si importa da `github.com/karavy/api-engine/pkg/apiengine`.

## Uso

```go
import (
    "net/http"
    "time"

    apiengine "github.com/karavy/api-engine/pkg/apiengine"
)

client, err := apiengine.NewClient(apiengine.Config{
    Timeout: 15 * time.Second,
    MaxResponseBytes: 5 << 20,
})
if err != nil {
    return err
}

response, err := client.Do(ctx, apiengine.Request{
    Method: http.MethodPost,
    Endpoint: "https://api.example.com/resources",
    Headers: http.Header{
        "Content-Type": {"application/json"},
        "Authorization": {"Bearer " + token},
    },
    Body: []byte(`{"name":"example"}`),
})
if err != nil {
    return err
}

// L'operator decide come gestire anche status 4xx/5xx.
// response.StatusCode, response.Headers, response.Body
```

Il frammento va inserito nella funzione chiamante: `ctx` è il suo `context.Context` e `token` proviene, ad esempio, da un Secret. Non serve creare un nuovo client per ogni riconciliazione.

## Collegamento alla CRD

L'operator mappa i campi della propria CRD su `Request`:

| Campo CRD (esempio) | Campo richiesta |
| --- | --- |
| `spec.method` | `Request.Method` |
| `spec.endpoint` | `Request.Endpoint` |
| `spec.body` | `Request.Body` |

Se il body è un `runtime.RawExtension`, passare `spec.Body.Raw`; se è un `json.RawMessage`, passarlo direttamente come body. Il payload viene inviato **senza serializzazioni aggiuntive**. Un body assente non invia alcun payload. La CRD e la riconciliazione rimangono responsabilità dell'operator.

## Configurazione e comportamento

- Endpoint assoluto `http://` o `https://`; metodo obbligatorio in maiuscolo, preferibilmente con le costanti di `net/http`.
- `Request.Headers` sovrascrive gli header predefiniti di `Config.Headers`. Specificare `Content-Type` esplicitamente: il body non deve necessariamente essere JSON.
- `Request.Query` (`url.Values`) aggiunge parametri alla query già presente nell'endpoint.
- Timeout predefinito: **30 secondi per tentativo**, inclusa la lettura della risposta. Il contesto permette cancellazione e una deadline totale per tentativi e attese.
- Risposta limitata a **10 MiB** per default, configurabile. Se il limite viene superato, vengono restituiti la risposta parziale e `ErrResponseTooLarge` (verificabile con `errors.Is`). Gli errori di lettura restituiscono anch'essi una risposta parziale: non trattarla come risultato completo.
- Gli status HTTP **4xx e 5xx non sono errori Go**. `Response.IsSuccess()` verifica gli status 2xx; `Response.DecodeJSON(&destination)` è disponibile per body JSON.
- Retry applicativi **opt-in** tramite `Config.Retry`, descritti sotto. Il transport standard di Go può ritentare autonomamente alcune richieste idempotenti in caso di connessioni riutilizzate non valide: `MaxAttempts` conta le chiamate applicative al client HTTP, non necessariamente ogni invio del transport.
- Per default i redirect **non vengono seguiti**: l'operator riceve lo status 3xx. Un `Config.HTTPClient` personalizzato conserva la propria policy di redirect.
- `Config.HTTPClient` consente transport personalizzati per TLS, certificati client, proxy e test. La configurazione viene copiata senza modificare il client originale; il transport è condiviso.
- Il client è riutilizzabile tra goroutine; non modificare body, header o query della richiesta mentre una chiamata è in corso.

## Retry e budget temporale

```go
client, err := apiengine.NewClient(apiengine.Config{
    Timeout: 5 * time.Second, // limite di ogni singolo tentativo
    Retry: apiengine.RetryConfig{
        MaxAttempts: 3, // primo tentativo + al massimo 2 retry
        InitialBackoff: 500 * time.Millisecond,
        MaxBackoff: 5 * time.Second,
    },
})
if err != nil {
    return err
}

// Richiede anche l'import di "context". ctx è il contesto del reconcile.
callCtx, cancel := context.WithTimeout(ctx, 20 * time.Second)
defer cancel()
response, err := client.Do(callCtx, request)
```

`request` è una `apiengine.Request` costruita dall'operator. Il timeout di `Config.Timeout` è **per tentativo**, non un budget complessivo: usare sempre una deadline del contesto per limitare tutta l'operazione, soprattutto quando il server invia un `Retry-After` elevato.

- `MaxAttempts` uguale a **0 o 1** disabilita i retry. Valori negativi o `MaxBackoff < InitialBackoff` sono errori di configurazione.
- Se omessi, `InitialBackoff` è **100 ms** e `MaxBackoff` è **5 s**. La base raddoppia a ogni retry fino al massimo; il jitter sceglie un'attesa tra il **50% e il 100%** della base, per distribuire le richieste di più operator.
- Status ritentabili: **408, 429, 500, 502, 503 e 504**. Gli altri status, inclusi 400, 401, 403, 404 e 409, sono restituiti subito.
- Errori ritentabili: timeout del singolo tentativo, connessioni interrotte/rifiutate, rete non raggiungibile e EOF durante il trasporto. Non vengono ritentati errori di input, certificati non validi, errori DNS permanenti, risposte parziali o troppo grandi.
- `Retry-After` accetta **secondi interi** o una **data HTTP**. Se valido, l'attesa è almeno quella richiesta dal server, anche se supera `MaxBackoff`; se assente, malformato o scaduto, resta il backoff normale. La cancellazione interrompe anche questa attesa.
- **GET, PUT e DELETE** sono idempotenti secondo HTTP e ammessi ai retry. Verificare comunque che l'API remota ne rispetti la semantica.
- Il body viene ricreato identico per ogni tentativo e ogni risposta viene letta entro il limite configurato e chiusa prima del tentativo successivo.
- Quando i tentativi si esauriscono, viene restituita l'ultima risposta HTTP **senza trasformare lo status in un errore Go**. L'ultimo errore di trasporto resta invece un errore. Se il contesto interrompe l'attesa, vengono restituiti l'ultima risposta disponibile e un errore verificabile con `errors.Is(err, context.Canceled)` o `errors.Is(err, context.DeadlineExceeded)`.

### POST e idempotenza

I **POST non vengono ritentati per default**, nemmeno con i retry abilitati. Per abilitarli servono **entrambe** queste condizioni:

1. `RetryConfig.RetryPOSTWithIdempotencyKey: true`.
2. Un header `Idempotency-Key` non vuoto nella richiesta o negli header predefiniti.

Abilitare questa opzione **solo se il server garantisce la deduplicazione tramite quella chiave**: la presenza dell'header da sola non rende sicuro un POST. La chiave rimane identica tra i tentativi. Nell'operator mantenerla stabile anche tra riconciliazioni della stessa operazione logica, ma cambiarla per un'operazione nuova; evitare una chiave globale condivisa tra richieste diverse.

I retry locali non sostituiscono il backoff di riconciliazione. Coordinare i due livelli per non moltiplicare i tentativi o bloccare a lungo i worker dell'operator.

## Sicurezza nell'operator

Un endpoint controllato da una CRD può consentire SSRF verso servizi interni o metadata cloud. Il modulo verifica la forma dell'URL, **non autorizza le destinazioni**: applicare nell'operator una allowlist di host/schemi/porte, controlli sugli indirizzi risolti e NetworkPolicy. Se si abilitano redirect, validare anche ogni destinazione del redirect.

Recuperare credenziali dai Secret invece di salvarle nel body o nell'URL. Non registrare indiscriminatamente header, payload, errori o risposte: possono contenere segreti. Usare HTTPS con verifica TLS; non disabilitarla in produzione.

## Verifica

```sh
go test -race ./...
go vet ./...
```

Richiede Go 1.22 o successivo. L'esempio eseguibile nei test mostra una chiamata completa usando un server locale.