package itinerary

import (
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
)

func mockExtCreate(t *testing.T) (*sqlx.DB, sqlmock.Sqlmock) {
	t.Helper()
	raw, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return sqlx.NewDb(raw, "postgres"), mock
}

// The 18-column add_tasks contract, pinned per intent method. Column ORDER and
// SET must match the legacy UpdateShift add_tasks INSERT exactly — rows born
// via the domain must be indistinguishable from pre-migration rows.
const addTasksCols = `INSERT INTO route_tasks \(id, shift_id, task_type, bin_id, potential_location_id, move_request_id, bin_number, latitude, longitude, address, destination_address, fill_percentage, sequence_order, is_completed, created_at, updated_at, added_by, addition_reason\) VALUES`

func TestAddCollection_ColumnContract(t *testing.T) {
	db, mock := mockExtCreate(t)
	defer db.Close()

	mock.ExpectExec(addTasksCols).
		WithArgs(sqlmock.AnyArg(), "s1", "collection",
			"b1", nil, nil,
			33, 37.1, -121.9,
			"1 Main St, San Jose 95112", nil,
			42, 7,
			0, int64(1700000000), int64(1700000000),
			"mgr1", "why").
		WillReturnResult(sqlmock.NewResult(0, 1))

	id, err := AddCollection(db, "s1", NewCollection{
		Seq: 7, BinID: "b1", BinNumber: 33, Lat: 37.1, Lng: -121.9,
		Address: "1 Main St, San Jose 95112", FillPercentage: 42,
		AddedBy: "mgr1", AdditionReason: "why", Now: 1700000000,
	})
	if err != nil || id == "" {
		t.Fatalf("AddCollection = (%q, %v), want (uuid, nil)", id, err)
	}
}

func TestAddPlacement_ColumnContract(t *testing.T) {
	db, mock := mockExtCreate(t)
	defer db.Close()

	mock.ExpectExec(addTasksCols).
		WithArgs(sqlmock.AnyArg(), "s1", "placement",
			nil, "pl1", nil,
			nil, 37.2, -121.8,
			"9 Place Blvd", nil,
			nil, 3,
			0, int64(1700000000), int64(1700000000),
			"mgr1", "why").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := AddPlacement(db, "s1", NewPlacement{
		Seq: 3, PotentialLocationID: "pl1", Lat: 37.2, Lng: -121.8,
		Address: "9 Place Blvd", AddedBy: "mgr1", AdditionReason: "why", Now: 1700000000,
	}); err != nil {
		t.Fatalf("AddPlacement: %v", err)
	}
}

// A dropoff leg: NULL address, destination_address set — the legacy quirk,
// preserved byte-for-byte in Slice 2a.
func TestAddMoveLeg_DropoffContract(t *testing.T) {
	db, mock := mockExtCreate(t)
	defer db.Close()

	dest := "P5 Dest Way"
	mock.ExpectExec(addTasksCols).
		WithArgs(sqlmock.AnyArg(), "s1", "dropoff",
			"b1", nil, "m1",
			nil, 37.3, -121.7,
			nil, &dest,
			nil, 5,
			0, int64(1700000000), int64(1700000000),
			"mgr1", "why").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := AddMoveLeg(db, "s1", NewMoveLeg{
		Seq: 5, Type: Dropoff, MoveRequestID: "m1", BinID: "b1",
		Lat: 37.3, Lng: -121.7, DestinationAddress: &dest,
		AddedBy: "mgr1", AdditionReason: "why", Now: 1700000000,
	}); err != nil {
		t.Fatalf("AddMoveLeg dropoff: %v", err)
	}
}

// Only pickup/dropoff are move legs — anything else is a programmer error.
func TestAddMoveLeg_RejectsNonLegType(t *testing.T) {
	db, _ := mockExtCreate(t)
	defer db.Close()

	if _, err := AddMoveLeg(db, "s1", NewMoveLeg{Type: Collection}); err == nil {
		t.Fatal("AddMoveLeg(collection) = nil error, want reject")
	}
}
