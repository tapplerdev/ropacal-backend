package models

type User struct {
	ID        string `json:"id" db:"id"`
	Email     string `json:"email" db:"email"`
	Password  string `json:"-" db:"password"` // Never return password in JSON
	Name      string `json:"name" db:"name"`
	Role      string `json:"role" db:"role"` // "driver" or "admin"
	CreatedAt int64  `json:"created_at" db:"created_at"`
	UpdatedAt int64  `json:"updated_at" db:"updated_at"`
}

type UserResponse struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	CreatedAt int64  `json:"created_at"`
}

func (u *User) ToUserResponse() UserResponse {
	return UserResponse{
		ID:        u.ID,
		Email:     u.Email,
		Name:      u.Name,
		Role:      u.Role,
		CreatedAt: u.CreatedAt,
	}
}
