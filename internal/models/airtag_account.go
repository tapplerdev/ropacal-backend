package models

// AirtagAccount represents an Apple iCloud account used by the FindMy bridge
type AirtagAccount struct {
	ID           string  `json:"id" db:"id"`
	Email        string  `json:"email" db:"email"`
	Password     string  `json:"password" db:"password"`
	AccountState *string `json:"account_state,omitempty" db:"account_state"`
	CreatedAt    int64   `json:"created_at" db:"created_at"`
	UpdatedAt    int64   `json:"updated_at" db:"updated_at"`
}

// AirtagKey represents a FindMy accessory key linked to an account
type AirtagKey struct {
	ID                    string  `json:"id" db:"id"`
	AccountID             string  `json:"account_id" db:"account_id"`
	Name                  string  `json:"name" db:"name"`
	TagUUID               string  `json:"tag_uuid" db:"tag_uuid"`
	PrivateKey            string  `json:"private_key" db:"private_key"`
	SharedSecret          string  `json:"shared_secret" db:"shared_secret"`
	SecondarySharedSecret string  `json:"secondary_shared_secret" db:"secondary_shared_secret"`
	PairingDate           *string `json:"pairing_date,omitempty" db:"pairing_date"`
	ProductID             *int64  `json:"product_id,omitempty" db:"product_id"`
	CreatedAt             int64   `json:"created_at" db:"created_at"`
}

// AirtagAccountWithKeys is the response shape sent to the bridge
type AirtagAccountWithKeys struct {
	ID           string      `json:"id"`
	Email        string      `json:"email"`
	Password     string      `json:"password"`
	AccountState *string     `json:"account_state,omitempty"`
	Keys         []AirtagKey `json:"keys"`
}
