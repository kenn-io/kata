package tracker

import "go.kenn.io/kata/internal/uid"

// Selection names a credential owner without carrying credentials or permission.
// Absence alone denotes a legacy binding; partial selections always fail closed.
type Selection struct {
	OwnerInstanceUID string `json:"owner_instance_uid" toml:"owner_instance_uid"`
	ConnectionUID    string `json:"connection_uid" toml:"connection_uid"`
	Generation       string `json:"generation" toml:"generation"`
}

func (c Selection) Validate() error {
	if !uid.Valid(c.OwnerInstanceUID) || !uid.Valid(c.ConnectionUID) || len(c.Generation) == 0 || len(c.Generation) > 128 {
		return ErrInvalid
	}
	for _, r := range c.Generation {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' && r != '.' {
			return ErrInvalid
		}
	}
	return nil
}
