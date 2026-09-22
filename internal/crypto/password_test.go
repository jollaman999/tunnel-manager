package crypto

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// The phrases the tests seal with. They are test input, not credentials.
const (
	testPhrase      = "tunnel-manager-test-phrase"
	testPhraseTypo  = "tunnel-manager-test-phrasf"
	testOtherPhrase = "another-test-phrase"
)

func TestEncryptWithPasswordRoundTrip(t *testing.T) {
	inputs := map[string]string{
		"empty":     "",
		"short":     "a",
		"korean":    "한글 설정값",
		"newlines":  "first line\nsecond line\r\nlast line\n",
		"json":      `{"host":"example","port":22}`,
		"megabyte":  strings.Repeat("0123456789abcdef", 65536),
		"non utf-8": string([]byte{0x00, 0xff, 0xfe, 0x41}),
	}

	for name, in := range inputs {
		t.Run(name, func(t *testing.T) {
			encrypted, err := EncryptWithPassword(in, testPhrase)
			if err != nil {
				t.Fatalf("EncryptWithPassword returned an error: %v", err)
			}

			if !IsEncryptedWithPassword(encrypted) {
				t.Fatal("EncryptWithPassword produced a value without the marker")
			}

			decrypted, err := DecryptWithPassword(encrypted, testPhrase)
			if err != nil {
				t.Fatalf("DecryptWithPassword returned an error: %v", err)
			}

			if decrypted != in {
				t.Errorf("the round trip changed the value of length %d", len(in))
			}
		})
	}
}

func TestEncryptWithPasswordUsesFreshSaltAndNonce(t *testing.T) {
	first, err := EncryptWithPassword("same-plaintext", testPhrase)
	if err != nil {
		t.Fatalf("EncryptWithPassword returned an error: %v", err)
	}

	second, err := EncryptWithPassword("same-plaintext", testPhrase)
	if err != nil {
		t.Fatalf("EncryptWithPassword returned an error: %v", err)
	}

	if first == second {
		t.Fatal("encrypting the same plaintext twice under the same password produced the same value")
	}

	// The salt sits in the header, right after the three parameter bytes, so a
	// shared salt shows up as a shared start of the body.
	firstBody := strings.TrimPrefix(first, passwordEncryptedPrefix)
	secondBody := strings.TrimPrefix(second, passwordEncryptedPrefix)
	if firstBody[:24] == secondBody[:24] {
		t.Fatal("two calls wrote the same salt")
	}
}

func TestDecryptWithWrongPasswordReportsWrongPassword(t *testing.T) {
	encrypted, err := EncryptWithPassword("the exported settings", testPhrase)
	if err != nil {
		t.Fatalf("EncryptWithPassword returned an error: %v", err)
	}

	for _, wrong := range []string{testPhraseTypo, testOtherPhrase, testPhrase + " "} {
		_, err = DecryptWithPassword(encrypted, wrong)
		if !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("decrypting with another password did not report a wrong password: %v", err)
		}
		if errors.Is(err, ErrNotPasswordEncrypted) || errors.Is(err, ErrPasswordEncryptedDamaged) {
			t.Fatalf("decrypting with another password blamed the file: %v", err)
		}
	}
}

func TestDecryptWithPasswordRejectsAlteredValue(t *testing.T) {
	encrypted, err := EncryptWithPassword("the exported settings", testPhrase)
	if err != nil {
		t.Fatalf("EncryptWithPassword returned an error: %v", err)
	}

	// One character in the middle of the body, replaced by another character of
	// the base64 alphabet so that the value still decodes and only the sealed
	// bytes differ. The authentication tag has to catch it.
	body := []byte(encrypted)
	middle := len(body) / 2
	if body[middle] == 'A' {
		body[middle] = 'B'
	} else {
		body[middle] = 'A'
	}

	altered := string(body)
	if altered == encrypted {
		t.Fatal("the test did not alter the value")
	}

	_, err = DecryptWithPassword(altered, testPhrase)
	if !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("an altered value was not rejected as unopenable: %v", err)
	}
}

func TestDecryptWithPasswordTellsForeignValuesApart(t *testing.T) {
	encrypted, err := EncryptWithPassword("the exported settings", testPhrase)
	if err != nil {
		t.Fatalf("EncryptWithPassword returned an error: %v", err)
	}

	installationEncrypted, err := newTestCipher(t).Encrypt("the stored password")
	if err != nil {
		t.Fatalf("Encrypt returned an error: %v", err)
	}

	// Nothing here carries the marker, so each one is a file that was never
	// written by this package, not a mistyped password.
	foreign := map[string]string{
		"empty":                   "",
		"arbitrary":               "not our file at all",
		"json":                    `{"hosts":[]}`,
		"the installation format": installationEncrypted,
		"the body alone":          strings.TrimPrefix(encrypted, passwordEncryptedPrefix),
	}

	for name, in := range foreign {
		t.Run(name, func(t *testing.T) {
			if IsEncryptedWithPassword(in) {
				t.Fatal("a foreign value carries the marker")
			}

			_, err := DecryptWithPassword(in, testPhrase)
			if !errors.Is(err, ErrNotPasswordEncrypted) {
				t.Fatalf("a foreign value was not reported as foreign: %v", err)
			}
			if errors.Is(err, ErrWrongPassword) {
				t.Fatal("a foreign value was reported as a wrong password")
			}
		})
	}
}

func TestDecryptWithPasswordReportsCutValueAsDamaged(t *testing.T) {
	encrypted, err := EncryptWithPassword("the exported settings", testPhrase)
	if err != nil {
		t.Fatalf("EncryptWithPassword returned an error: %v", err)
	}

	// A file that was cut keeps the marker, so it is ours and damaged. Cutting
	// by a length that is not a multiple of four breaks the base64, and cutting
	// the body down to the marker leaves nothing to read.
	cut := map[string]string{
		"cut by ten":        encrypted[:len(encrypted)-10],
		"cut to the salt":   encrypted[:len(passwordEncryptedPrefix)+8],
		"cut to the marker": passwordEncryptedPrefix,
	}

	for name, in := range cut {
		t.Run(name, func(t *testing.T) {
			_, err := DecryptWithPassword(in, testPhrase)
			if !errors.Is(err, ErrPasswordEncryptedDamaged) {
				t.Fatalf("a cut value was not reported as damaged: %v", err)
			}
			if errors.Is(err, ErrWrongPassword) || errors.Is(err, ErrNotPasswordEncrypted) {
				t.Fatalf("a cut value was reported as something else: %v", err)
			}
		})
	}
}

func TestDecryptWithPasswordRejectsUnusableParameters(t *testing.T) {
	// A file states the memory its own reading takes, so a file that names more
	// than this build allows has to be refused before scrypt allocates it.
	for name, header := range map[string][3]byte{
		"N of 2^0":  {0, scryptR, scryptP},
		"r of zero": {scryptLogN, 0, scryptP},
		"p of zero": {scryptLogN, scryptR, 0},
		"N of 2^40": {40, scryptR, scryptP},
		"N of 2^63": {63, scryptR, scryptP},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkScryptParameters(int(header[0]), int(header[1]), int(header[2]))
			if !errors.Is(err, ErrPasswordEncryptedDamaged) {
				t.Fatalf("unusable scrypt parameters were not reported as damaged: %v", err)
			}
		})
	}

	err := checkScryptParameters(scryptLogN, scryptR, scryptP)
	if err != nil {
		t.Fatalf("the parameters this package writes were refused: %v", err)
	}
}

// passwordValueWithParameters writes a value that states the given parameters
// and carries a body long enough to be read past the nonce and the tag, so that
// what a test sees is the verdict on the parameters and not on the length.
func passwordValueWithParameters(logN byte, r byte, p byte) string {
	body := make([]byte, 0, passwordHeaderSize+44)
	body = append(body, logN, r, p)
	body = append(body, make([]byte, passwordSaltSize+44)...)

	return passwordEncryptedPrefix + base64.StdEncoding.EncodeToString(body)
}

func TestDecryptWithPasswordCapsTheMemoryAFileAsksFor(t *testing.T) {
	// scrypt takes 128 * r * N bytes, so with r of 8 a stated N of 2^17 is the
	// 128 MiB that maxScryptMemory allows and 2^19 is four times that.
	_, err := DecryptWithPassword(passwordValueWithParameters(19, scryptR, scryptP), testPhrase)
	if !errors.Is(err, ErrPasswordEncryptedDamaged) {
		t.Fatalf("a file that asks for 512 MiB was not refused as damaged: %v", err)
	}

	// At the cap the parameters are the last thing standing between the file
	// and its body, so a file that sits on it is read that far and fails on the
	// body instead: this one is zeros and does not authenticate.
	_, err = DecryptWithPassword(passwordValueWithParameters(17, scryptR, scryptP), testPhrase)
	if !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("a file at the cap was not read as far as its body: %v", err)
	}
}

func TestDecryptWithPasswordOpensOneAtATime(t *testing.T) {
	encrypted, err := EncryptWithPassword("the exported settings", testPhrase)
	if err != nil {
		t.Fatalf("EncryptWithPassword returned an error: %v", err)
	}

	// Taking the slot the way an open in flight holds it leaves the open below
	// waiting on that one thing, with no timing of two real opens to read.
	passwordOpenSlots <- struct{}{}

	started := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		close(started)

		_, err := DecryptWithPassword(encrypted, testPhrase)
		done <- err
	}()

	<-started

	// An open under the parameters this package writes took 62 ms when it was
	// measured, so one that did not wait its turn would have finished several
	// times over before this is up.
	select {
	case err = <-done:
		t.Fatalf("a second open ran while the first held the slot: %v", err)
	case <-time.After(time.Second):
	}

	<-passwordOpenSlots

	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("the open that waited returned an error: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the second open did not run after the slot was given back")
	}
}

func TestPasswordEncryptionRejectsAnEmptyPassword(t *testing.T) {
	_, err := EncryptWithPassword("the exported settings", "")
	if !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("EncryptWithPassword accepted an empty password: %v", err)
	}

	encrypted, err := EncryptWithPassword("the exported settings", testPhrase)
	if err != nil {
		t.Fatalf("EncryptWithPassword returned an error: %v", err)
	}

	_, err = DecryptWithPassword(encrypted, "")
	if !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("DecryptWithPassword accepted an empty password: %v", err)
	}
}

func TestEncryptWithPasswordWritesItsParameters(t *testing.T) {
	encrypted, err := EncryptWithPassword("the exported settings", testPhrase)
	if err != nil {
		t.Fatalf("EncryptWithPassword returned an error: %v", err)
	}

	// The reader picks the layout by the version in the marker, so a change of
	// the marker is a change of the format and not an edit of this test.
	if !strings.HasPrefix(encrypted, "tmpwenc:v1:") {
		t.Fatalf("the marker is not the v1 one: %q", encrypted[:len(passwordEncryptedPrefix)])
	}

	// The value must not be mistaken for the one the installation key writes.
	if IsEncrypted(encrypted) {
		t.Fatal("a password encrypted value carries the marker of the installation key format")
	}
}

func BenchmarkEncryptWithPassword(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, err := EncryptWithPassword("the exported settings", testPhrase)
		if err != nil {
			b.Fatalf("EncryptWithPassword returned an error: %v", err)
		}
	}
}

func BenchmarkDecryptWithPassword(b *testing.B) {
	encrypted, err := EncryptWithPassword("the exported settings", testPhrase)
	if err != nil {
		b.Fatalf("EncryptWithPassword returned an error: %v", err)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := DecryptWithPassword(encrypted, testPhrase)
		if err != nil {
			b.Fatalf("DecryptWithPassword returned an error: %v", err)
		}
	}
}
