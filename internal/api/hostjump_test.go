package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/jollaman999/tunnel-manager/internal/models"
)

// registerJumpHosts registers count Hosts with no route, numbered from 1.
func registerJumpHosts(t *testing.T, f *hostFixture, count int) {
	t.Helper()

	for i := 1; i <= count; i++ {
		rec := f.createHost(t, fmt.Sprintf(`{"address":"192.0.2.%d","port":22,"user":"operator","password":"secret"}`, i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
		}
	}
}

// answeredRoute is the jump_host_ids of the Host an answer carries.
func answeredRoute(t *testing.T, body []byte) []uint {
	t.Helper()

	var answer struct {
		Data struct {
			JumpHostIDs []uint `json:"jump_host_ids"`
		} `json:"data"`
	}

	err := json.Unmarshal(body, &answer)
	if err != nil {
		t.Fatalf("failed to read the answer %s: %v", body, err)
	}
	if answer.Data.JumpHostIDs == nil {
		t.Fatalf("the answer carries no jump_host_ids list: %s", body)
	}

	return answer.Data.JumpHostIDs
}

// storedRoute is the route of hostID as the rows hold it, in seq order, with
// the seq checked to count from 1.
func storedRoute(t *testing.T, f *hostFixture, hostID uint) []uint {
	t.Helper()

	var rows []models.HostJump

	err := f.db.Where("host_id = ?", hostID).Order("seq").Find(&rows).Error
	if err != nil {
		t.Fatalf("failed to read the route: %v", err)
	}

	route := make([]uint, 0, len(rows))
	for i, row := range rows {
		if row.Seq != uint(i+1) {
			t.Fatalf("the route of Host %d is stored as %+v, want seq counted from 1", hostID, rows)
		}
		route = append(route, row.JumpHostID)
	}

	return route
}

func jumpRefusalOf(t *testing.T, body []byte) errorBody {
	t.Helper()

	var answer errorBody

	err := json.Unmarshal(body, &answer)
	if err != nil {
		t.Fatalf("failed to read the refusal %s: %v", body, err)
	}

	return answer
}

func TestAJumpRouteIsStoredAndAnsweredInItsOrder(t *testing.T) {
	f := newHostFixture(t)
	registerJumpHosts(t, f, 3)

	rec := f.createHost(t, `{"address":"198.51.100.1","port":22,"user":"operator","password":"secret","jump_host_ids":[3,1,2]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	want := []uint{3, 1, 2}

	if got := answeredRoute(t, rec.Body.Bytes()); !reflect.DeepEqual(got, want) {
		t.Fatalf("the create answer carries %v, want %v", got, want)
	}
	if got := storedRoute(t, f, 4); !reflect.DeepEqual(got, want) {
		t.Fatalf("the stored route is %v, want %v", got, want)
	}

	rec = f.call(t, http.MethodGet, "/api/host/4", "", "id", "4", f.h.GetHost)
	if got := answeredRoute(t, rec.Body.Bytes()); !reflect.DeepEqual(got, want) {
		t.Fatalf("GetHost answers %v, want %v", got, want)
	}

	rec = f.call(t, http.MethodGet, "/api/host", "", "", "", f.h.ListHosts)

	var page struct {
		Data struct {
			Items []hostView `json:"items"`
		} `json:"data"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &page)
	if err != nil {
		t.Fatalf("failed to read the list %s: %v", rec.Body.String(), err)
	}
	if len(page.Data.Items) != 4 {
		t.Fatalf("the list carries %d Hosts, want 4: %s", len(page.Data.Items), rec.Body.String())
	}

	for _, view := range page.Data.Items {
		wanted := []uint{}
		if view.ID == 4 {
			wanted = want
		}
		if !reflect.DeepEqual(view.JumpHostIDs, wanted) {
			t.Errorf("the list carries %v for Host %d, want %v", view.JumpHostIDs, view.ID, wanted)
		}
	}
}

func TestAHostWithNoRouteAnswersAnEmptyList(t *testing.T) {
	f := newHostFixture(t)
	registerJumpHosts(t, f, 1)

	rec := f.call(t, http.MethodGet, "/api/host/1", "", "id", "1", f.h.GetHost)
	if got := answeredRoute(t, rec.Body.Bytes()); len(got) != 0 {
		t.Fatalf("GetHost answers %v, want an empty list", got)
	}
}

func TestAnUpdateKeepsTheRouteItLeavesOutAndClearsAnEmptyOne(t *testing.T) {
	f := newHostFixture(t)
	registerJumpHosts(t, f, 3)

	rec := f.updateHost(t, "3", `{"jump_host_ids":[2,1]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := storedRoute(t, f, 3); !reflect.DeepEqual(got, []uint{2, 1}) {
		t.Fatalf("the stored route is %v, want [2 1]", got)
	}

	for _, body := range []string{`{"description":"changed"}`, `{"description":"again","jump_host_ids":null}`} {
		rec = f.updateHost(t, "3", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want %d, body: %s", body, rec.Code, http.StatusOK, rec.Body.String())
		}
		if got := answeredRoute(t, rec.Body.Bytes()); !reflect.DeepEqual(got, []uint{2, 1}) {
			t.Fatalf("%s: the answer carries %v, want the route that is stored", body, got)
		}
		if got := storedRoute(t, f, 3); !reflect.DeepEqual(got, []uint{2, 1}) {
			t.Fatalf("%s: the stored route is %v, want it kept", body, got)
		}
	}

	rec = f.updateHost(t, "3", `{"jump_host_ids":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := answeredRoute(t, rec.Body.Bytes()); len(got) != 0 {
		t.Fatalf("the answer carries %v, want an empty list", got)
	}
	if got := storedRoute(t, f, 3); len(got) != 0 {
		t.Fatalf("the stored route is %v, want none", got)
	}
}

func TestADisabledHostIsTakenAsAJump(t *testing.T) {
	f := newHostFixture(t)

	rec := f.createHost(t, `{"address":"192.0.2.1","port":22,"user":"operator","password":"secret","enabled":false}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	rec = f.createHost(t, `{"address":"192.0.2.2","port":22,"user":"operator","password":"secret","jump_host_ids":[1]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
}

func TestAJumpRouteThatCannotBeTakenIsRefused(t *testing.T) {
	tests := []struct {
		name   string
		create bool
		route  string
		code   errorCode
		args   errorArgs
	}{
		{"unknown on create", true, "[1,99]", errHostJumpUnknown, errorArgs{"host_id": "99"}},
		{"unknown on update", false, "[99]", errHostJumpUnknown, errorArgs{"host_id": "99"}},
		{"self", false, "[1,10]", errHostJumpSelf, nil},
		{"repeated on create", true, "[1,2,1]", errHostJumpRepeated, errorArgs{"host_id": "1"}},
		{"repeated on update", false, "[2,2]", errHostJumpRepeated, errorArgs{"host_id": "2"}},
		{"too many", true, "[1,2,3,4,5,6,7,8,9]", errHostJumpTooMany, errorArgs{"max": "8"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHostFixture(t)
			registerJumpHosts(t, f, 10)

			var rec *httptest.ResponseRecorder
			if tt.create {
				rec = f.createHost(t, `{"address":"198.51.100.1","port":22,"user":"operator","password":"secret","jump_host_ids":`+tt.route+`}`)
			} else {
				rec = f.updateHost(t, "10", `{"jump_host_ids":`+tt.route+`}`)
			}

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}

			refusal := jumpRefusalOf(t, rec.Body.Bytes())
			if refusal.Code != tt.code || !reflect.DeepEqual(refusal.Args, tt.args) {
				t.Fatalf("refused with %s %v, want %s %v", refusal.Code, refusal.Args, tt.code, tt.args)
			}

			if f.hostCount(t) != 10 {
				t.Fatalf("%d Hosts are stored after the refusal, want 10", f.hostCount(t))
			}
			if got := storedRoute(t, f, 10); len(got) != 0 {
				t.Fatalf("the refused route was stored as %v", got)
			}
		})
	}
}

// Eight is the longest route there may be, and a route of eight is taken.
func TestARouteOfEightIsTaken(t *testing.T) {
	f := newHostFixture(t)
	registerJumpHosts(t, f, 8)

	rec := f.createHost(t, `{"address":"198.51.100.1","port":22,"user":"operator","password":"secret","jump_host_ids":[1,2,3,4,5,6,7,8]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
}

func TestAHostOnARouteIsNotDeleted(t *testing.T) {
	f := newHostFixture(t)
	registerJumpHosts(t, f, 3)

	for _, id := range []string{"2", "3"} {
		rec := f.updateHost(t, id, `{"jump_host_ids":[1]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	rec := f.call(t, http.MethodDelete, "/api/host/1", "", "id", "1", f.h.DeleteHost)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	var refusal struct {
		Code errorCode  `json:"error_code"`
		Args errorArgs  `json:"error_args"`
		Data []jumpUser `json:"data"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &refusal)
	if err != nil {
		t.Fatalf("failed to read the refusal %s: %v", rec.Body.String(), err)
	}

	wantArgs := errorArgs{"host_ids": "2,3", "hosts": "192.0.2.2:22, 192.0.2.3:22"}
	if refusal.Code != errHostDeleteUsedAsJump || !reflect.DeepEqual(refusal.Args, wantArgs) {
		t.Fatalf("refused with %s %v, want %s %v", refusal.Code, refusal.Args, errHostDeleteUsedAsJump, wantArgs)
	}

	wantData := []jumpUser{{ID: 2, Address: "192.0.2.2", Port: 22}, {ID: 3, Address: "192.0.2.3", Port: 22}}
	if !reflect.DeepEqual(refusal.Data, wantData) {
		t.Fatalf("the refusal carries %+v, want %+v", refusal.Data, wantData)
	}

	if f.hostCount(t) != 3 {
		t.Fatalf("%d Hosts are stored after the refusal, want 3", f.hostCount(t))
	}
}

func TestADeletedHostTakesItsRouteWithIt(t *testing.T) {
	f := newHostFixture(t)
	registerJumpHosts(t, f, 3)

	err := f.db.AutoMigrate(&models.LocalForward{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	for _, update := range []struct{ id, route string }{{"2", "[1]"}, {"3", "[1]"}} {
		rec := f.updateHost(t, update.id, `{"jump_host_ids":`+update.route+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	rec := f.call(t, http.MethodDelete, "/api/host/2", "", "id", "2", f.h.DeleteHost)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := storedRoute(t, f, 2); len(got) != 0 {
		t.Fatalf("the route of the deleted Host is left as %v", got)
	}
	if got := storedRoute(t, f, 3); !reflect.DeepEqual(got, []uint{1}) {
		t.Fatalf("the route of another Host is %v, want [1]", got)
	}
}
