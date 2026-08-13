package httpapi

import (
	"fmt"
	"os/user"
)

func currentUserID() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current user: %w", err)
	}
	if current.Uid == "" {
		return "", fmt.Errorf("resolve current user: empty user identifier")
	}
	return current.Uid, nil
}
