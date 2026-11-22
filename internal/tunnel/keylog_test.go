package tunnel

import (
	"strings"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestReconcileLogsAKeyThatCannotBeUsedOncePerKey covers a Host with three
// service ports whose key cannot be used and that has no password to fall back
// on, over three passes. Every pass tries all three tunnels again, and each
// try that fails says why on its own line. What is wrong with the key is said
// once for a key that cannot be read, since the answer is kept and is the same
// every time, and again for a passphrase replaced by another that does not
// open the key either. A value the encryption key in use cannot open is said
// once for each try, since it is not kept and is opened once for each.
func TestReconcileLogsAKeyThatCannotBeUsedOncePerKey(t *testing.T) {
	const ports = 3
	const passes = 3

	cases := []struct {
		name string
		// otherCipher seals the Host with an encryption key other than the
		// one in use.
		otherCipher bool
		passphrase  string
		// replaceBefore is the pass before which the passphrase is replaced
		// by another wrong one, counted from 1, or 0 for none.
		replaceBefore int
		// perPass is how many lines with each id a pass is to write, by pass.
		perPass []map[logid.ID]int
	}{
		{
			name:          "wrong passphrase",
			passphrase:    "not the passphrase",
			replaceBefore: 3,
			perPass: []map[logid.ID]int{
				{logid.TunnelHostKeyUnreadable: 1, logid.TunnelStartFailed: ports},
				{logid.TunnelStartFailed: ports},
				{logid.TunnelHostKeyUnreadable: 1, logid.TunnelStartFailed: ports},
			},
		},
		{
			name:        "another encryption key",
			otherCipher: true,
			passphrase:  "the passphrase of the test",
			perPass: []map[logid.ID]int{
				{logid.TunnelHostSecretUndecryptable: ports, logid.TunnelStartFailed: ports},
				{logid.TunnelHostSecretUndecryptable: ports, logid.TunnelStartFailed: ports},
				{logid.TunnelHostSecretUndecryptable: ports, logid.TunnelStartFailed: ports},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cipher := newTestCipher(t)
			sealWith := cipher
			if tc.otherCipher {
				sealWith = newTestCipher(t)
			}

			keyPEM, _ := testPrivateKey(t, "the passphrase of the test")
			host := sealedHost(t, sealWith, keyPEM, tc.passphrase, "")
			host.Port = 1

			hosts := []models.Host{*host}
			db := newStubDB(t, hosts, servicePortsOf(ports), nil)
			core, logs := observer.New(zapcore.DebugLevel)

			m, err := NewManager(db, zap.New(core), cipher, 1)
			if err != nil {
				t.Fatalf("failed to create manager: %v", err)
			}
			t.Cleanup(m.StopAllTunnels)

			for pass := 0; pass < passes; pass++ {
				if pass+1 == tc.replaceBefore {
					replaced := sealedHost(t, sealWith, keyPEM, "nor is this the passphrase", "")
					hosts[0].KeyPassphrase = replaced.KeyPassphrase
				}

				before := logs.Len()

				result, err := m.Reconcile()
				if err != nil {
					t.Fatalf("pass %d failed: %v", pass+1, err)
				}
				if result.Failed != ports {
					t.Fatalf("pass %d failed %d tunnels, want %d", pass+1, result.Failed, ports)
				}

				lines := logs.All()[before:]
				got := make(map[logid.ID]int)
				for _, line := range lines {
					id, _ := line.ContextMap()[logid.FieldKey].(string)
					got[logid.ID(id)]++

					if logid.ID(id) == logid.TunnelStartFailed {
						cause, _ := line.ContextMap()["error"].(string)
						if !strings.Contains(cause, "private key") {
							t.Fatalf("pass %d: a failed start does not say why: %q", pass+1, cause)
						}
					}
				}

				want := tc.perPass[pass]
				t.Logf("pass %d wrote %d lines: %v", pass+1, len(lines), got)

				wantTotal := 0
				for id, n := range want {
					wantTotal += n
					if got[id] != n {
						t.Errorf("pass %d wrote %d lines of %s, want %d", pass+1, got[id], id, n)
					}
				}
				if len(lines) != wantTotal {
					t.Errorf("pass %d wrote %d lines, want %d", pass+1, len(lines), wantTotal)
				}
			}
		})
	}
}
