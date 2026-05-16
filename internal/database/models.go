package database

import (
	"time"
)

// User represents a user in the database
type User struct {
	ID         int        `db:"id"`
	AccessKey  string     `db:"access_key"`
	SecretKey  string     `db:"secret_key"`
	Email      string     `db:"email"`
	CreatedAt  time.Time  `db:"created_at"`
	LastLogin  *time.Time `db:"last_login"`
	Active     bool       `db:"active"`
	OrgID      *string    `db:"org_id"`
	Role       string     `db:"role"`
}

// UserPermission represents bucket-level permissions for a user
type UserPermission struct {
	ID            int    `db:"id"`
	UserID        int    `db:"user_id"`
	BucketPattern string `db:"bucket_pattern"`
	Permissions   string `db:"permissions"` // comma-separated: read,write,delete
}

// Organization represents a tenant organization
type Organization struct {
	ID        string    `db:"id"`
	Name      string    `db:"name"`
	Slug      string    `db:"slug"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
	Active    bool      `db:"active"`
	Settings  string    `db:"settings"`
}

// OrgBucketMapping represents a virtual-to-physical bucket mapping for an org
type OrgBucketMapping struct {
	ID             string    `db:"id" json:"id"`
	OrgID          string    `db:"org_id" json:"org_id"`
	VirtualBucket  string    `db:"virtual_bucket" json:"virtual_bucket"`
	PhysicalBucket string    `db:"physical_bucket" json:"physical_bucket"`
	Prefix         string    `db:"prefix" json:"prefix"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
}