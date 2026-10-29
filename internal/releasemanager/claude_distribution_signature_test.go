package releasemanager

import (
	"strings"
	"testing"
)

// Public release metadata retrieved from the official CDN. This exercises the
// production pinned-key path without a test signing key or network access.
func TestClaudeDistributionVerifiesOfficialSignedManifest(t *testing.T) {
	if err := verifyClaudeManifest([]byte(claudeSignedManifestFixture), []byte(claudeSignatureFixture)); err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(claudeSignedManifestFixture, `"version": "2.1.312"`, `"version": "2.1.313"`, 1)
	if err := verifyClaudeManifest([]byte(tampered), []byte(claudeSignatureFixture)); err == nil {
		t.Fatal("accepted a modified signed Claude manifest")
	}
}

const claudeSignedManifestFixture = `{
  "version": "2.1.312",
  "manifestSignatureEnforcement": "flag",
  "commit": "f375cd85d4f70920de17ce3a4ad646e18834e833",
  "buildDate": "2026-10-27T17:16:03Z",
  "platforms": {
    "darwin-arm64": {
      "binary": "claude",
      "checksum": "6b5ec691195e551b02c41df9f7d723f4ea61eaa03c2df90acefe3736f1aeb620",
      "size": 275988512
    },
    "darwin-x64": {
      "binary": "claude",
      "checksum": "9534770e162cfd6e39301d9ab775affce214df032a68484085bdbab966a7f8cf",
      "size": 287239296
    },
    "linux-arm64": {
      "binary": "claude",
      "checksum": "dc4af28917455e009d26704b82a438353a06149e825b6cdf782908b2d28661f5",
      "size": 293462200
    },
    "linux-x64": {
      "binary": "claude",
      "checksum": "4f4178ce41d83e23341ebfbaecdbcf8ea6bf7baea0690025abe49eeec3a5d01d",
      "size": 293592432
    },
    "linux-arm64-musl": {
      "binary": "claude",
      "checksum": "62555d1d87e4fff49298b70f306ef851fa167fc58b179958680f7c4d407a8c5c",
      "size": 285625544
    },
    "linux-x64-musl": {
      "binary": "claude",
      "checksum": "78adef63924b762580b039c9ff0dd2ae44eb06171d467db62fd9040442505f76",
      "size": 287258072
    },
    "win32-x64": {
      "binary": "claude.exe",
      "checksum": "6099672ebe5dcb518b67d1d32dde1e3360bc6bee1dae7f8d28a69c49acc7c865",
      "size": 298690208
    },
    "win32-arm64": {
      "binary": "claude.exe",
      "checksum": "2bde7020ff94a52b59fa4c0c14f81558d3c5180848b2d85069b6efcad0415a4f",
      "size": 282754208
    }
  },
  "sdkCompat": {
    "testedWrapperVersions": [
      "0.3.280",
      "0.3.281",
      "0.3.282",
      "0.3.283",
      "0.3.284",
      "0.3.285",
      "0.3.286",
      "0.3.287",
      "0.3.288",
      "0.3.289",
      "0.3.290",
      "0.3.291",
      "0.3.292",
      "0.3.293",
      "0.3.294",
      "0.3.295",
      "0.3.296",
      "0.3.299",
      "0.3.300",
      "0.3.301",
      "0.3.302",
      "0.3.303",
      "0.3.304",
      "0.3.305",
      "0.3.306"
    ],
    "harnessSchema": 1
  }
}
`

const claudeSignatureFixture = `-----BEGIN PGP SIGNATURE-----

iQIzBAABCgAdFiEEMd3eJN36tnn0LXvSuqkp/xp+ys4FAmrg3NYACgkQuqkp/xp+
ys7SWA/+IhrkOZ6z+Sq+g6XpufiOiRTrOHcVLqPA7H5+S7DkW83DaXEWYi3l1m/v
+6LogxNj7nGFrZ/2EewvVw8KOCkvBF8pGmOURkprPkWq5dMHlSDUw9JYWae57w0m
Izw3kuxC9QpqdkQv02YPFK8oaHr0fjPisjA+vsYCqZCb+r3l03oCYZbtiz3ylkN/
pWzUZAi9YmcYOlpHvdqNFN/+XXKH0iM0ihn50/y02DXEjHJyNCkfK+j8oEZjwcy0
HTt10Tz38XqhL2zmvnpMk9uEf2V0yGkOGtk7wXK1GnpZBmhVjJhg2KT9fGKJcI0d
sFQqT6409TtFyixdmIOsOsKL2Ihy2wyG0jXg+ETIHi3FNOZOaZAoyxP/lpvLXySz
EFHzS3i9gtibkCSlpWVNoSG6lo7khZi5Tkrto+07s3BUH752e4QnAudlMqMuhAXO
3xJN0r4e64J93/fFj6kFgWLTnVkUbQFBec0zmYFTUH2eDjDq9QQHo6jATuo+Z/VP
AWo9Mi78sRv3vgCvhxZC/7nFJLyF+nK2glamdd7FTgeBognajGd9bcvfvOhAS/fO
Hca50pMIP5m7x8p9eEOQggPyaaNaBUt9iWDXqcN8lDiZJBGgrGY8axHrIlslZJDF
H2MkMQVAjjapnZLhE7Hzc56XZo2ylD8449dwu3aSt+C+CLwB8AU=
=TxfC
-----END PGP SIGNATURE-----
`
