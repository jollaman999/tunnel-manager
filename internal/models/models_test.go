package models

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// oldTunnel is Tunnel as it stood before the forwarded port was measured. It is
// what the tunnels table of an installation migrated by an earlier release
// looks like, and it is what the migration test below builds its database from.
type oldTunnel struct {
	HostID          uint   `gorm:"primaryKey;not null"`
	SPID            uint   `gorm:"primaryKey;not null"`
	Status          string `gorm:"not null"`
	LastError       string
	RetryCount      int `gorm:"default:0"`
	LastConnectedAt time.Time
	Server          string `gorm:"not null"`
	Local           string `gorm:"not null"`
	Remote          string `gorm:"not null"`
}

func (oldTunnel) TableName() string {
	return "tunnels"
}

// TestTunnelMigrationKeepsTheRowsThatWereThere pins that the two readings added
// to Tunnel reach an installation that has rows already. AutoMigrate adds a
// column to a table that is in use, and a column that is added NOT NULL with no
// default is what breaks the rows that are there, so neither of the two carries
// one. What a row written before them holds is nothing, which is the same thing
// the screen and the API take for a reading that was never made.
func TestTunnelMigrationKeepsTheRowsThatWereThere(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tm.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&oldTunnel{})
	if err != nil {
		t.Fatalf("failed to build the table as it was: %v", err)
	}

	before := oldTunnel{
		HostID:     1,
		SPID:       2,
		Status:     "connected",
		LastError:  "",
		RetryCount: 3,
		Server:     "192.0.2.10:22",
		Local:      "0.0.0.0:18080",
		Remote:     "198.51.100.20:8080",
	}

	err = db.Create(&before).Error
	if err != nil {
		t.Fatalf("failed to write a row of the table as it was: %v", err)
	}

	err = db.AutoMigrate(&Tunnel{})
	if err != nil {
		t.Fatalf("failed to migrate the table: %v", err)
	}

	var after Tunnel
	err = db.Where("host_id = ? AND sp_id = ?", before.HostID, before.SPID).First(&after).Error
	if err != nil {
		t.Fatalf("the row that was there before the migration cannot be read: %v", err)
	}

	if after.Status != before.Status || after.RetryCount != before.RetryCount ||
		after.Server != before.Server || after.Local != before.Local || after.Remote != before.Remote {
		t.Fatalf("the migration changed the row that was there: %+v", after)
	}

	if after.ServerBanner != "" || after.ForwardReach != "" {
		t.Fatalf("a row written before the readings carries one: banner=%q reach=%q",
			after.ServerBanner, after.ForwardReach)
	}

	after.ServerBanner = "SSH-2.0-OpenSSH_10.5p1"
	after.ForwardReach = "unreachable"

	err = db.Save(&after).Error
	if err != nil {
		t.Fatalf("failed to write a reading to the migrated row: %v", err)
	}

	var stored Tunnel
	err = db.Where("host_id = ? AND sp_id = ?", before.HostID, before.SPID).First(&stored).Error
	if err != nil {
		t.Fatalf("failed to read the migrated row back: %v", err)
	}

	if stored.ServerBanner != after.ServerBanner || stored.ForwardReach != after.ForwardReach {
		t.Fatalf("the readings did not survive being stored: banner=%q reach=%q",
			stored.ServerBanner, stored.ForwardReach)
	}
}

// oldHost is Host as it stood before the address the forwarded ports are opened
// on could be chosen. It is what the hosts table of an installation migrated by
// an earlier release looks like.
// oldHostServicePort is the assignment table as the releases before this one
// built it: the pair that is the key and the time it was made, and nothing
// about where the forwarded port of the pair is opened. It is written out here
// rather than migrated from the model, which carries the column now, because
// the shape it left behind is what a running installation holds.
type oldHostServicePort struct {
	HostID    uint `gorm:"primaryKey;not null"`
	SPID      uint `gorm:"primaryKey;not null"`
	CreatedAt time.Time
}

func (oldHostServicePort) TableName() string {
	return "host_service_ports"
}

// newAssignmentTable opens a database holding the assignments alone. The two
// tables the pair points at are not there, because nothing in this file asks
// the database to follow the pair to a row: what is under test is the column
// the assignment carries.
func newAssignmentTable(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tm.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&HostServicePort{})
	if err != nil {
		t.Fatalf("failed to build the table: %v", err)
	}

	return db
}

// storedBindScope is what the table holds for one assignment.
func storedBindScope(t *testing.T, db *gorm.DB, hostID, spID uint) string {
	t.Helper()

	var stored HostServicePort
	err := db.Where("host_id = ? AND sp_id = ?", hostID, spID).First(&stored).Error
	if err != nil {
		t.Fatalf("failed to read the assignment of host %d with service port %d: %v", hostID, spID, err)
	}

	return stored.BindScope
}

// TestAnAssignmentIsOpenToEverythingUntilItIsToldOtherwise is what keeps an
// upgrade from changing how far a running tunnel reaches.
//
// The column is added to a table that is in use, and a row written before it
// existed comes through holding nothing. Nothing has to go on meaning the
// wildcard, which is what those rows were already being forwarded on, so the
// column carries no NOT NULL and no default and nothing rewrites what is
// stored: an installation that upgrades and touches no assignment opens
// exactly the ports it opened before.
func TestAnAssignmentIsOpenToEverythingUntilItIsToldOtherwise(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tm.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&oldHostServicePort{})
	if err != nil {
		t.Fatalf("failed to build the table as it was: %v", err)
	}

	before := oldHostServicePort{HostID: 1, SPID: 2, CreatedAt: time.Now().UTC()}

	err = db.Create(&before).Error
	if err != nil {
		t.Fatalf("failed to write a row of the table as it was: %v", err)
	}

	err = db.AutoMigrate(&HostServicePort{})
	if err != nil {
		t.Fatalf("failed to migrate the table: %v", err)
	}

	var after HostServicePort
	err = db.Where("host_id = ? AND sp_id = ?", before.HostID, before.SPID).First(&after).Error
	if err != nil {
		t.Fatalf("the row that was there before the migration cannot be read: %v", err)
	}

	if after.BindScope != "" {
		t.Fatalf("a row written before the column carries the bind scope %q, "+
			"so the migration narrowed a tunnel that is running", after.BindScope)
	}

	// An assignment made now and asked nothing about the scope is the same
	// wildcard. That is every caller written before the column existed.
	err = db.Create(&HostServicePort{HostID: 3, SPID: 4}).Error
	if err != nil {
		t.Fatalf("an assignment that names no bind scope was refused: %v", err)
	}

	if scope := storedBindScope(t, db, 3, 4); scope != "" {
		t.Fatalf("an assignment that names no bind scope was stored as %q", scope)
	}
}

// TestOneHostOpensItsServicePortsDifferently is half of why the scope is on the
// assignment. One Host carries several service ports, and one of them may
// belong to that machine alone while the next is to be reached from elsewhere.
func TestOneHostOpensItsServicePortsDifferently(t *testing.T) {
	db := newAssignmentTable(t)

	for _, assignment := range []HostServicePort{
		{HostID: 1, SPID: 1, BindScope: BindScopeLoopback},
		{HostID: 1, SPID: 2, BindScope: BindScopeWildcard},
	} {
		err := db.Create(&assignment).Error
		if err != nil {
			t.Fatalf("the assignment of service port %d was refused: %v", assignment.SPID, err)
		}
	}

	if scope := storedBindScope(t, db, 1, 1); scope != BindScopeLoopback {
		t.Fatalf("the first service port of the Host is open to %q, want %q", scope, BindScopeLoopback)
	}
	if scope := storedBindScope(t, db, 1, 2); scope != BindScopeWildcard {
		t.Fatalf("the second service port of the Host is open to %q, want %q", scope, BindScopeWildcard)
	}
}

// TestOneServicePortIsOpenedDifferentlyOnTwoHosts is the other half. The same
// service port is carried by several Hosts, and only some of them face a
// network nobody else should reach in over.
func TestOneServicePortIsOpenedDifferentlyOnTwoHosts(t *testing.T) {
	db := newAssignmentTable(t)

	for _, assignment := range []HostServicePort{
		{HostID: 1, SPID: 1, BindScope: BindScopeLoopback},
		{HostID: 2, SPID: 1, BindScope: BindScopeWildcard},
	} {
		err := db.Create(&assignment).Error
		if err != nil {
			t.Fatalf("the assignment on host %d was refused: %v", assignment.HostID, err)
		}
	}

	if scope := storedBindScope(t, db, 1, 1); scope != BindScopeLoopback {
		t.Fatalf("the service port on the first Host is open to %q, want %q", scope, BindScopeLoopback)
	}
	if scope := storedBindScope(t, db, 2, 1); scope != BindScopeWildcard {
		t.Fatalf("the service port on the second Host is open to %q, want %q", scope, BindScopeWildcard)
	}
}

// TestABindScopeThatIsNeitherIsRefused pins that the rule is in the database
// rather than in whatever was about to write the row. Assignments are written
// as model values from several places, and a value that is neither of the two
// words has no answer to the question of which addresses to ask for: it would
// sit in the table until a tunnel is built and be read then, by something with
// nothing to do but guess.
func TestABindScopeThatIsNeitherIsRefused(t *testing.T) {
	db := newAssignmentTable(t)

	refused := []string{
		// An address is what the release before this one stored, one Host at a
		// time. The column holds what was chosen and not an address.
		"127.0.0.1",
		"0.0.0.0",
		"LOOPBACK",
		"anywhere",
	}

	for i, scope := range refused {
		err := db.Create(&HostServicePort{HostID: uint(i + 1), SPID: 1, BindScope: scope}).Error
		if err == nil {
			t.Fatalf("an assignment was stored with the bind scope %q", scope)
		}
	}

	for i, scope := range []string{"", BindScopeLoopback, BindScopeWildcard} {
		err := db.Create(&HostServicePort{HostID: uint(i + 10), SPID: 1, BindScope: scope}).Error
		if err != nil {
			t.Fatalf("an assignment with the bind scope %q was refused: %v", scope, err)
		}
	}
}

// TestTunnelSerializesTheForwardedPortReadings pins the two names the API
// answers with. The status screen reads them off the tunnel rows of
// GET /api/status, so a rename here is a screen that shows nothing.
func TestTunnelSerializesTheForwardedPortReadings(t *testing.T) {
	encoded, err := json.Marshal(Tunnel{
		HostID:       1,
		SPID:         2,
		Status:       "connected",
		ServerBanner: "SSH-2.0-OpenSSH_10.5p1 Ubuntu-1ubuntu2",
		ForwardReach: "unreachable",
	})
	if err != nil {
		t.Fatalf("failed to serialize: %v", err)
	}

	body := string(encoded)

	for _, want := range []string{
		`"server_banner":"SSH-2.0-OpenSSH_10.5p1 Ubuntu-1ubuntu2"`,
		`"forward_reach":"unreachable"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the serialized Tunnel is missing %s: %s", want, body)
		}
	}
}

// TestHostServicePortHoldsOnePairOnce pins that the two columns are the primary
// key together rather than one of them being it. The table is a set of pairs,
// and a key on HostID alone would let the second service port of a Host
// overwrite the first one while the same pair could be stored twice.
func TestHostServicePortHoldsOnePairOnce(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "tm.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	err = db.AutoMigrate(&HostServicePort{})
	if err != nil {
		t.Fatalf("failed to build the table: %v", err)
	}

	// The same Host with two service ports, and the same service port on two
	// Hosts. Neither is a repeat of a pair, so both belong in the table.
	for _, pair := range []HostServicePort{
		{HostID: 1, SPID: 1},
		{HostID: 1, SPID: 2},
		{HostID: 2, SPID: 1},
	} {
		err = db.Create(&pair).Error
		if err != nil {
			t.Fatalf("host %d with service port %d was refused: %v", pair.HostID, pair.SPID, err)
		}
	}

	var count int64
	err = db.Model(&HostServicePort{}).Count(&count).Error
	if err != nil {
		t.Fatalf("failed to count the assignments: %v", err)
	}
	if count != 3 {
		t.Fatalf("%d assignments are stored, want the 3 that were written", count)
	}

	err = db.Create(&HostServicePort{HostID: 1, SPID: 2}).Error
	if err == nil {
		t.Fatal("the same pair was stored twice")
	}

	var stored HostServicePort
	err = db.Where("host_id = ? AND sp_id = ?", 1, 2).First(&stored).Error
	if err != nil {
		t.Fatalf("failed to read an assignment back: %v", err)
	}
	if stored.CreatedAt.IsZero() {
		t.Fatal("the assignment was stored without the time it was made")
	}
}

// TestCreateRequestsTellAnAbsentAssignmentFromFalse is why the two assignment
// fields are pointers. A request that asks for no assignments and one that does
// not mention them mean opposite things: the first is a row that carries
// nothing on purpose, the second is every client written before the field
// existed, and what those clients ran assigned everything. On a plain bool both
// arrive as false and the second would quietly be given the first one's answer,
// which is the trap the Enabled field on Host was already caught in.
func TestCreateRequestsTellAnAbsentAssignmentFromFalse(t *testing.T) {
	const noField = `{"ip":"192.0.2.10","port":22,"user":"operator"}`

	var host CreateHostRequest

	err := json.Unmarshal([]byte(noField), &host)
	if err != nil {
		t.Fatalf("failed to read the request: %v", err)
	}
	if host.AssignAllServicePorts != nil {
		t.Fatalf("a Host that says nothing about the assignments arrives as %v, which cannot be told "+
			"from one that asked for none", *host.AssignAllServicePorts)
	}

	err = json.Unmarshal([]byte(`{"assign_all_service_ports":false}`), &host)
	if err != nil {
		t.Fatalf("failed to read the request: %v", err)
	}
	if host.AssignAllServicePorts == nil || *host.AssignAllServicePorts {
		t.Fatalf("a Host that asked for no assignments arrives as %v, want a stored false",
			host.AssignAllServicePorts)
	}

	var sp CreateServicePortRequest

	err = json.Unmarshal([]byte(`{"service_ip":"198.51.100.10","service_port":80,"local_port":8080}`), &sp)
	if err != nil {
		t.Fatalf("failed to read the request: %v", err)
	}
	if sp.AssignToAllHosts != nil {
		t.Fatalf("a service port that says nothing about the assignments arrives as %v, which cannot "+
			"be told from one that asked for none", *sp.AssignToAllHosts)
	}

	err = json.Unmarshal([]byte(`{"assign_to_all_hosts":false}`), &sp)
	if err != nil {
		t.Fatalf("failed to read the request: %v", err)
	}
	if sp.AssignToAllHosts == nil || *sp.AssignToAllHosts {
		t.Fatalf("a service port that asked for no assignments arrives as %v, want a stored false",
			sp.AssignToAllHosts)
	}
}
