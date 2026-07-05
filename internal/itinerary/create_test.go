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

// The move-leg column contract (Slice 2b): the 18 legacy columns PLUS
// destination_latitude/longitude and move_type — the pickup's destination_*
// is what makes the move visible to the OR-Tools shipment builder (#34).
const legCols = `INSERT INTO route_tasks \(id, shift_id, task_type, bin_id, potential_location_id, move_request_id, bin_number, latitude, longitude, address, destination_address, destination_latitude, destination_longitude, move_type, fill_percentage, sequence_order, is_completed, created_at, updated_at, added_by, addition_reason\) VALUES`

// A pickup leg carries the move's destination — the #34 fix pinned.
func TestAddMoveLeg_PickupCarriesDestination(t *testing.T) {
	db, mock := mockExtCreate(t)
	defer db.Close()

	origin, dest := "P5 Move Rd", "P5 Dest Way"
	dLat, dLng, fill, binNum := 37.334, -121.884, 42, 10993
	mock.ExpectExec(legCols).
		WithArgs(sqlmock.AnyArg(), "s1", "pickup",
			"b1", nil, "m1",
			&binNum, 37.332, -121.882,
			&origin, &dest,
			&dLat, &dLng, "relocation",
			&fill, 4,
			0, int64(1700000000), int64(1700000000),
			"mgr1", "why").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := AddMoveLeg(db, "s1", NewMoveLeg{
		Seq: 4, Type: Pickup, MoveRequestID: "m1", BinID: "b1",
		BinNumber: &binNum, FillPercentage: &fill, MoveType: "relocation",
		Lat: 37.332, Lng: -121.882, Address: &origin,
		DestLat: &dLat, DestLng: &dLng, DestinationAddress: &dest,
		AddedBy: "mgr1", AdditionReason: "why", Now: 1700000000,
	}); err != nil {
		t.Fatalf("AddMoveLeg pickup: %v", err)
	}
}

// A dropoff leg sits AT the destination: its own coords + address duplicate
// the destination_* columns (the app-nav convention shared with AddMove).
func TestAddMoveLeg_DropoffContract(t *testing.T) {
	db, mock := mockExtCreate(t)
	defer db.Close()

	dest := "P5 Dest Way"
	dLat, dLng, binNum := 37.334, -121.884, 10993
	mock.ExpectExec(legCols).
		WithArgs(sqlmock.AnyArg(), "s1", "dropoff",
			"b1", nil, "m1",
			&binNum, 37.334, -121.884,
			&dest, &dest,
			&dLat, &dLng, "relocation",
			nil, 5,
			0, int64(1700000000), int64(1700000000),
			"mgr1", "why").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := AddMoveLeg(db, "s1", NewMoveLeg{
		Seq: 5, Type: Dropoff, MoveRequestID: "m1", BinID: "b1",
		BinNumber: &binNum, MoveType: "relocation",
		Lat: 37.334, Lng: -121.884, Address: &dest,
		DestLat: &dLat, DestLng: &dLng, DestinationAddress: &dest,
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
