# Vendored assets

Third-party assets are committed rather than fetched at build or run time, so
the binary is self-contained (DEPLOY-04) and the Content-Security-Policy can
forbid any external origin.

| Asset | Version | Source | SHA-256 |
|---|---|---|---|
| htmx.min.js | 2.0.4 | https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js | e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447 |

To update: download the pinned URL, record the new digest here, and verify the
digest matches the publisher's published value before committing.
