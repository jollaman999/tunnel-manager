package api

import (
	"database/sql"
	"strings"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// readListSearch reads the text a list request narrows its rows by, which is
// q on the query string. No q, or an empty one, narrows nothing: the list is
// answered exactly as it was before the parameter was there.
func readListSearch(c echo.Context) string {
	return c.QueryParam("q")
}

// likeContaining is the LIKE pattern that matches a column holding text
// anywhere in it. The three characters LIKE gives a meaning to are escaped
// with a backslash, which every query here names in its ESCAPE clause, so that
// a search for 50% or for db_1 finds those characters and not whatever a
// wildcard would.
//
// The pattern is bound as an argument and never written into the SQL. SQLite
// folds case in LIKE for the ASCII letters, which is what makes the match
// case-insensitive on addresses and on what is written in them.
func likeContaining(text string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)

	return "%" + escaped + "%"
}

// hostsMatching narrows a read of the Hosts to the ones whose address, user,
// description or SSH port holds q. The port is matched as the text it is
// written as, so that 22 finds a Host on 2222 the way it would on the screen.
func hostsMatching(q string) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		if q == "" {
			return db
		}

		return db.Where(`ip LIKE @q ESCAPE '\' OR "user" LIKE @q ESCAPE '\' OR `+
			`description LIKE @q ESCAPE '\' OR CAST(port AS TEXT) LIKE @q ESCAPE '\'`,
			sql.Named("q", likeContaining(q)))
	}
}

// servicePortsMatching narrows a read of the service ports to the ones whose
// service address, service port, local port or description holds q, the ports
// matched as text for the reason hostsMatching gives.
func servicePortsMatching(q string) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		if q == "" {
			return db
		}

		return db.Where(`service_ip LIKE @q ESCAPE '\' OR CAST(service_port AS TEXT) LIKE @q ESCAPE '\' OR `+
			`CAST(local_port AS TEXT) LIKE @q ESCAPE '\' OR description LIKE @q ESCAPE '\'`,
			sql.Named("q", likeContaining(q)))
	}
}

// statusHostsMatchingSQL is the Hosts a row of the status table is found by:
// those whose address or description holds @q. It is the one condition over
// the Hosts both sorts of row are matched on, so that a search for a Host
// finds its tunnels and its local forwards alike.
const statusHostsMatchingSQL = `SELECT id FROM hosts WHERE ip LIKE @q ESCAPE '\' OR description LIKE @q ESCAPE '\'`

// tunnelsMatching narrows a read of the tunnel rows to the ones whose Host
// matches q, or whose local or remote address holds it. The same narrowing is
// put on the read of the references and on the read of the page, so that the
// LIMIT and OFFSET the page is read with are counted over the rows that match
// and not over the table.
func tunnelsMatching(q string) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		if q == "" {
			return db
		}

		return db.Where(`local LIKE @q ESCAPE '\' OR remote LIKE @q ESCAPE '\' OR host_id IN (`+
			statusHostsMatchingSQL+`)`,
			sql.Named("q", likeContaining(q)))
	}
}

// localForwardsMatching reads every local forward whose Host matches q, or
// whose listen address or target holds it, in the order the table is paged
// in.
//
// The addresses of a forward are not columns. They are what
// tunnel.LocalForwardAddresses makes of the bind scope and the ports, and
// writing that out again in SQL would be a second place keeping the rule. So
// the rows are read whole and matched here, on the addresses the status row
// carries. That is every forward, which is a few dozen rows, and the page is
// then cut out of this list rather than read again: what the answer counts and
// what it hands over are the one read.
//
// The addresses hold nothing but ASCII, so folding them with strings.ToLower
// matches them the way LIKE matches the Hosts.
func localForwardsMatching(db *gorm.DB, q string) ([]models.LocalForward, error) {
	var hostIDs []uint

	err := db.Raw(statusHostsMatchingSQL, sql.Named("q", likeContaining(q))).Scan(&hostIDs).Error
	if err != nil {
		return nil, err
	}

	hostMatches := make(map[uint]bool, len(hostIDs))
	for _, id := range hostIDs {
		hostMatches[id] = true
	}

	var forwards []models.LocalForward

	err = db.Order("host_id, number").Find(&forwards).Error
	if err != nil {
		return nil, err
	}

	folded := strings.ToLower(q)
	matched := make([]models.LocalForward, 0, len(forwards))

	for i := range forwards {
		lf := forwards[i]

		if hostMatches[lf.HostID] {
			matched = append(matched, lf)

			continue
		}

		listenV4, _, _, target := tunnel.LocalForwardAddresses(&models.Host{}, &lf)
		if strings.Contains(strings.ToLower(listenV4), folded) || strings.Contains(strings.ToLower(target), folded) {
			matched = append(matched, lf)
		}
	}

	return matched, nil
}

// localForwardRefsOf is statusLocalForwardRefs over forwards already read.
func localForwardRefsOf(forwards []models.LocalForward) []statusRef {
	refs := make([]statusRef, 0, len(forwards))

	for _, lf := range forwards {
		refs = append(refs, statusRef{HostID: lf.HostID, Kind: statusKindLocalForward, Ref: lf.Number})
	}

	return refs
}
