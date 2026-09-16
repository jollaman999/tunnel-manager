package models

import (
	"encoding/json"
	"strings"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestUserTableNameIsSingular(t *testing.T) {
	name := User{}.TableName()

	if name != "user" {
		t.Fatalf("the table name is %q, want %q", name, "user")
	}
}

// newDryRunDB returns a gorm handle over the mysql dialector that builds
// statements without sending them, so the SQL a model produces can be read
// without a database.
func newDryRunDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "user:password@tcp(127.0.0.1:3306)/tunnel-manager",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DryRun: true, DisableAutomaticPing: true})
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
