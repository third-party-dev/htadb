# TLS

TLS matters only for `https://` listeners. With plain `http://` or `unix://`
listeners htadb never creates or reads TLS files. Loopback HTTP
(`http://127.0.0.1:…`) is already a secure context in browsers, so it needs no
certificate.

## Certificates

- `--tls-cert` and `--tls-key` (PEM) take precedence over everything below.
- Otherwise htadb keeps a local CA and a leaf certificate in
  `<config dir>/tls/` (`~/.config/htadb/tls/` on Linux, honouring
  `XDG_CONFIG_HOME`; `~/Library/Application Support/htadb/tls/` on macOS;
  `%AppData%\htadb\tls\` on Windows; `--config-dir` moves it):

  | File | |
  | --- | --- |
  | `ca.pem`, `ca-key.pem` | Local CA, created once and kept (10 years) |
  | `cert.pem`, `key.pem` | Leaf signed by the CA (1 year); `cert.pem` includes the CA |

  Keys are ECDSA P-256; files are `0600`, the directory `0700`.
- The leaf's SANs are `localhost`, `127.0.0.1`, `::1`, the hostname, the
  https listener IPs (all interface addresses for `0.0.0.0`/`::`) and every
  `--tls-san` (DNS name or IP).
- The leaf is regenerated when its SANs change, when it is within 30 days of
  expiry, or when it was not signed by the current CA. The CA is reused, so
  trust survives renewals.

```sh
htadb tls print-ca      # CA path and SHA-256 fingerprint
htadb tls regenerate    # force a new leaf (keeps the CA)
```

## Trusting the CA

Trust `ca.pem` once per machine or browser profile; compare the fingerprint
with `htadb tls print-ca`.

**Linux (system store, Debian/Ubuntu)**

```sh
sudo cp ~/.config/htadb/tls/ca.pem /usr/local/share/ca-certificates/htadb-ca.crt
sudo update-ca-certificates
```

Fedora/RHEL: copy to `/etc/pki/ca-trust/source/anchors/` and run
`sudo update-ca-trust`. Chrome and Chromium on Linux use their own NSS store:

```sh
certutil -d sql:$HOME/.pki/nssdb -A -t "C,," -n htadb -i ~/.config/htadb/tls/ca.pem
```

**macOS**

```sh
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain \
  ~/Library/Application\ Support/htadb/tls/ca.pem
```

**Windows** (PowerShell, current user)

```powershell
Import-Certificate -FilePath "$env:APPDATA\htadb\tls\ca.pem" -CertStoreLocation Cert:\CurrentUser\Root
```

**Chrome** (any OS, without touching the system store): Settings → Privacy
and security → Security → Manage certificates → Authorities → Import
`ca.pem`, and tick "Trust this certificate for identifying websites".

**Firefox**: Settings → Privacy & Security → Certificates → View
Certificates → Authorities → Import.

## Behind a reverse proxy

Run htadb on loopback or a unix socket and let the proxy terminate TLS. The
proxy must forward WebSocket upgrades (`/api/v1/devices/track`,
`/api/v1/devices/*/shell`, `/api/v1/devices/*/mirror`, `/api/v1/cli`).
Example (Caddy):

```
adb.example.com {
	reverse_proxy unix//run/htadb.sock
}
```
