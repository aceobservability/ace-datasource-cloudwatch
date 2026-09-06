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

`cfg.URL` is a custom AWS endpoint when it is not an `*.amazonaws.com` /
`*.amazon.com` host (httptest, LocalStack). Custom endpoints require static
`access_key_id` and `secret_access_key` so Ace does not SigV4-sign them with
the host default credential chain. Production URLs such as
`https://monitoring.us-east-1.amazonaws.com` leave endpoint resolution to the
SDK so metrics and logs keep their service-specific hosts.

## Tests

```
go test ./...
```

Query and connection tests speak to an `httptest` fixture. No live AWS is
required.
