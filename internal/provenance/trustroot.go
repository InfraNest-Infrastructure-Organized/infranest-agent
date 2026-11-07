package provenance

import "time"

// The Sigstore public-good trust anchors, embedded rather than fetched (InfraNest #5969).
//
// Copied from `trusted_root.json` as Sigstore's TUF repository published it on 2026-11-07 — sha256
// 6494e21ea73fa7ee769f85f57d5a3e6a08725eae1e38c755fc3517c9e6bc0b66, the same bytes at
// https://tuf-repo-cdn.sigstore.dev (targets v14) and in github.com/sigstore/root-signing. Only the entries
// a GitHub artifact attestation from this repository is checked against are here: the current Fulcio CA,
// the Rekor log and the certificate-transparency log it submits to. Retired ones are left out on purpose —
// a release signed today cannot have been signed under them.
//
// Embedded, not fetched, because the alternative is a TUF client: a second trust bootstrap, a refresh
// schedule, a cache on disk and another host to reach, all to protect a check that runs a few times a
// year. The cost of embedding is that a rotation on Sigstore's side needs an agent release before it
// reaches the signer — and release.yml checks every new release against the previous release's verifier
// before publishing it, so that day is found by the release, not by a customer's upgrade.

// fulcioChain is the Fulcio CA that issues GitHub Actions' signing certificates: the intermediate first,
// then the root. Valid for certificates issued from 2022-04-13.
var fulcioChain = []string{
	"MIICGjCCAaGgAwIBAgIUALnViVfnU0brJasmRkHrn/UnfaQwCgYIKoZIzj0EAwMwKjEVMBMGA1UEChMMc2lnc3RvcmUuZGV2MREwDwYDVQQDEwhzaWdzdG9yZTAeFw0yMjA0MTMyMDA2MTVaFw0zMTEwMDUxMzU2NThaMDcxFTATBgNVBAoTDHNpZ3N0b3JlLmRldjEeMBwGA1UEAxMVc2lnc3RvcmUtaW50ZXJtZWRpYXRlMHYwEAYHKoZIzj0CAQYFK4EEACIDYgAE8RVS/ysH+NOvuDZyPIZtilgUF9NlarYpAd9HP1vBBH1U5CV77LSS7s0ZiH4nE7Hv7ptS6LvvR/STk798LVgMzLlJ4HeIfF3tHSaexLcYpSASr1kS0N/RgBJz/9jWCiXno3sweTAOBgNVHQ8BAf8EBAMCAQYwEwYDVR0lBAwwCgYIKwYBBQUHAwMwEgYDVR0TAQH/BAgwBgEB/wIBADAdBgNVHQ4EFgQU39Ppz1YkEZb5qNjpKFWixi4YZD8wHwYDVR0jBBgwFoAUWMAeX5FFpWapesyQoZMi0CrFxfowCgYIKoZIzj0EAwMDZwAwZAIwPCsQK4DYiZYDPIaDi5HFKnfxXx6ASSVmERfsynYBiX2X6SJRnZU84/9DZdnFvvxmAjBOt6QpBlc4J/0DxvkTCqpclvziL6BCCPnjdlIB3Pu3BxsPmygUY7Ii2zbdCdliiow=",
	"MIIB9zCCAXygAwIBAgIUALZNAPFdxHPwjeDloDwyYChAO/4wCgYIKoZIzj0EAwMwKjEVMBMGA1UEChMMc2lnc3RvcmUuZGV2MREwDwYDVQQDEwhzaWdzdG9yZTAeFw0yMTEwMDcxMzU2NTlaFw0zMTEwMDUxMzU2NThaMCoxFTATBgNVBAoTDHNpZ3N0b3JlLmRldjERMA8GA1UEAxMIc2lnc3RvcmUwdjAQBgcqhkjOPQIBBgUrgQQAIgNiAAT7XeFT4rb3PQGwS4IajtLk3/OlnpgangaBclYpsYBr5i+4ynB07ceb3LP0OIOZdxexX69c5iVuyJRQ+Hz05yi+UF3uBWAlHpiS5sh0+H2GHE7SXrk1EC5m1Tr19L9gg92jYzBhMA4GA1UdDwEB/wQEAwIBBjAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBRYwB5fkUWlZql6zJChkyLQKsXF+jAfBgNVHSMEGDAWgBRYwB5fkUWlZql6zJChkyLQKsXF+jAKBggqhkjOPQQDAwNpADBmAjEAj1nHeXZp+13NWBNa+EDsDP8G1WWg1tCMWP/WHPqpaVo0jhsweNFZgSs0eE7wYI4qAjEA2WB9ot98sIkoF3vZYdd3/VtWB5b9TNMea7Ix/stJ5TfcLLeABLE4BNJOsQ4vnBHJ",
}

var fulcioValidFrom = time.Date(2022, 4, 13, 20, 6, 15, 0, time.UTC)

// rekorKey is rekor.sigstore.dev's signing key (ECDSA P-256, PKIX), whose SHA-256 is the log id
// wNI9atQGlz+VWfO6LRygH4QUfY/8W4RFwiT5i5WRgB0=. It signs both the inclusion promise and the checkpoint.
const rekorKey = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2G2Y+2tabdTV5BcGiBIx0a9fAFwrkBbmLSGtks4L3qX6yYY0zufBnhC8Ur/iy55GhWP/9A/bY2LhC30M9+RYtw=="

// rekorOrigin is the first line of every checkpoint that key signs.
const rekorOrigin = "rekor.sigstore.dev"

var rekorValidFrom = time.Date(2021, 1, 12, 11, 53, 27, 0, time.UTC)

// ctLogKey is ctfe.sigstore.dev/2022 (ECDSA P-256, PKIX), the certificate-transparency log Fulcio submits
// every certificate to before issuing it. Its SHA-256 is the log id
// 3T0wasbHETJjGR4cmWc3AqJKXrjePK3/h4pygC8p7o4=.
const ctLogKey = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEiPSlFi0CmFTfEjCUqF9HuCEcYXNKAaYalIJmBZ8yyezPjTqhxrKBpMnaocVtLJBI1eM3uXnQzQGAJdJ4gs9Fyw=="

var ctLogValidFrom = time.Date(2022, 10, 20, 0, 0, 0, 0, time.UTC)
