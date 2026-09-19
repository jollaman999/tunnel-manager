package models

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestUserTableNameIsSingular(t *testing.T) {
	name := User{}.TableName()

	if name != "user" {
		t.Fatalf("the table name is %q, want %q", name, "user")
	}
}

// newDryRunDB returns a gorm handle over the SQLite dialector that builds
// statements without sending them, so the SQL a model produces can be read
// without a database file. The dialector asks the database for its version as
// it is opened, which DryRun does not cover, so the handle is pointed at an
// in-memory one and nothing else ever reaches it.
func newDryRunDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
	if err != nil {
		t.Fatalf("failed to open a dry run handle: %v", err)
	}

	return db
}

// TestUserTableNameIsQuotedInSQL pins that "user", which is also the name of an
// SQL function, reaches the database as an identifier.
func TestUserTableNameIsQuotedInSQL(t *testing.T) {
	db := newDryRunDB(t)

	var count int64
	countSQL := db.Model(&User{}).Count(&count).Statement.SQL.String()

	var user User
	findSQL := db.Where("id = ?", 1).Find(&user).Statement.SQL.String()

	for _, sql := range []string{countSQL, findSQL} {
		if !strings.Contains(sql, "`user`") {
			t.Fatalf("the table name is not quoted with backticks in %q", sql)
		}
		if strings.Contains(sql, "`users`") {
			t.Fatalf("the table name was pluralized in %q", sql)
		}
	}
}

func TestUserPasswordHashIsNotSerialized(t *testing.T) {
	// A value that is obviously not a hash, so that the test says nothing about
	// what a stored hash looks like.
	user := User{ID: 1, Username: "test-user", PasswordHash: "not-a-real-hash", SetupRequired: true}

	encoded, err := json.Marshal(user)
	if err != nil {
		t.Fatalf("failed to serialize: %v", err)
	}

	body := string(encoded)

	if strings.Contains(body, "password_hash") {
		t.Fatalf("the serialized User carries a password_hash field: %s", body)
	}
	if strings.Contains(body, "PasswordHash") {
		t.Fatalf("the serialized User carries a PasswordHash field: %s", body)
	}
	if strings.Contains(body, user.PasswordHash) {
		t.Fatalf("the serialized User carries the stored hash")
	}
	if !strings.Contains(body, `"username":"test-user"`) {
		t.Fatalf("the serialized User is missing the username: %s", body)
	}
}

// TestHostSecretsAreNotSerialized is what keeps the way into every machine a
// Host names inside the process. The private key is worth more than the
// password beside it: a key that leaves here opens every Host that trusts it,
// and the passphrase next to it takes off what protects it at rest.
func TestHostSecretsAreNotSerialized(t *testing.T) {
	// Values that are obviously not secrets, so the test says nothing about
	// what a stored one looks like.
	host := Host{
		ID:            1,
		IP:            "192.0.2.10",
		Port:          22,
		User:          "operator",
		Password:      "not-a-real-password",
		PrivateKey:    "not-a-real-key",
		KeyPassphrase: "not-a-real-passphrase",
		Description:   "the host of the test",
		Enabled:       true,
	}

	encoded, err := json.Marshal(host)
	if err != nil {
		t.Fatalf("failed to serialize: %v", err)
	}

	body := string(encoded)

	for _, name := range []string{
		"password", "Password",
		"private_key", "PrivateKey",
		"key_passphrase", "KeyPassphrase",
	} {
		if strings.Contains(body, name) {
			t.Fatalf("the serialized Host carries a %s field: %s", name, body)
		}
	}

	for _, value := range []string{host.Password, host.PrivateKey, host.KeyPassphrase} {
		if strings.Contains(body, value) {
			t.Fatalf("the serialized Host carries a stored secret: %s", body)
		}
	}

	if !strings.Contains(body, `"ip":"192.0.2.10"`) {
		t.Fatalf("the serialized Host is missing the address: %s", body)
	}
}

// TestHostPasswordColumnIsNullable pins that a Host may be stored without a
// password. It is the one part of the schema the private key changed: the
// column used to be NOT NULL, from when a password was the only way in, and a
// Host that carries a key alone has none.
func TestHostPasswordColumnIsNullable(t *testing.T) {
	stmt := &gorm.Statement{DB: newDryRunDB(t)}
	err := stmt.Parse(&Host{})
	if err != nil {
		t.Fatalf("failed to parse the Host schema: %v", err)
	}

	for _, name := range []string{"password", "private_key", "key_passphrase"} {
		field := stmt.Schema.LookUpField(name)
		if field == nil {
			t.Fatalf("the Host schema has no %s column", name)
		}
		if field.NotNull {
			t.Fatalf("the %s column is NOT NULL, so a Host without one cannot be stored", name)
		}
	}
}
