package models

// BinCheckRecommendation represents a recommendation for a bin that needs checking
// Simple time-based system: bins not checked in 7+ days are flagged (no priority levels)
type BinCheckRecommendation struct {
	ID               string  `json:"id" db:"id"`
	BinID            string  `json:"bin_id" db:"bin_id"`
	Reason           string  `json:"reason" db:"reason"` // 'time_based' or 'manual_flag'
	FlaggedAt        int64   `json:"flagged_at" db:"flagged_at"`
	DaysSinceCheck   int     `json:"days_since_check" db:"days_since_check"`
	Status           string  `json:"status" db:"status"` // 'pending', 'resolved', 'dismissed'
	ResolvedAt       *int64  `json:"resolved_at,omitempty" db:"resolved_at"`
	ResolvedByUserID *string `json:"resolved_by_user_id,omitempty" db:"resolved_by_user_id"`
	Notes            *string `json:"notes,omitempty" db:"notes"`
	CreatedAt        int64   `json:"created_at" db:"created_at"`
	UpdatedAt        int64   `json:"updated_at" db:"updated_at"`
}

// BinCheckRecommendationWithBin includes bin details for API responses
type BinCheckRecommendationWithBin struct {
	BinCheckRecommendation
	Bin *Bin `json:"bin,omitempty"`
}
