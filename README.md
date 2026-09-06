# ace-datasource-cloudwatch

Compile-time CloudWatch datasource module for [Ace](https://github.com/aceobservability/ace).

Ace keeps the datasource contract and registry in
`github.com/aceobservability/ace/backend/pkg/datasource`. This module implements
that `Client` (plus `QueryWithSignal` and connection test). Ace registers the
factory at `init` and injects its SSRF-safe HTTP client — this module does not
import Ace `internal/` packages and does not construct an unpolicy'd client.

## Contract

| Surface | Package |
| --- | --- |
| Query / result types | `github.com/aceobservability/ace/backend/pkg/datasource` |
| Registry type key | `cloudwatch` (`Type`) |
| Factory | `New(cfg datasource.Config, httpClient *http.Client)` |

`httpClient` is required. Ace passes `ssrf.DatasourceClient` wrapped with stored
datasource credentials. The AWS SDK uses that client for CloudWatch Metrics and
Logs API calls.

Query and Test Connection require static `access_key_id` and
`secret_access_key`. The host default credential chain (env, shared profile,
instance role) is never used.

`cfg.URL` is a custom AWS endpoint when it is not an `*.amazonaws.com` /
`*.amazon.com` host (httptest, LocalStack). Production URLs such as
`https://monitoring.us-east-1.amazonaws.com` leave endpoint resolution to the
SDK so metrics and logs keep their service-specific hosts. Amazon hosts never
set a shared BaseEndpoint.

## Tests

```
go test ./...
```

Query and connection tests speak to an `httptest` fixture. No live AWS is
required.
