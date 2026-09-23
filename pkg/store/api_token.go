package store

import (
	"database/sql"
	"errors"
	"regexp"
)

var (
	ErrInvalidAPIToken  = errors.New("Token 须为 1–256 位字母、数字或 . _ ~ -，且不能使用保留值 joycode、default")
	ErrAPITokenConflict = errors.New("Token 已被其他账户的 Token 或用户 ID 使用")
	apiTokenPattern     = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,256}$`)
)

// UpdateAPIToken replaces an account's routing token atomically. Restricting
// characters also keeps tokens safe in the dashboard's generated shell commands.
func (s *Store) UpdateAPIToken(userID, token string) error {
	if !apiTokenPattern.MatchString(token) || token == "joycode" || token == "default" {
		return ErrInvalidAPIToken
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow("SELECT 1 FROM accounts WHERE user_id = ?", userID).Scan(&exists); err != nil {
		return err
	}
	err = tx.QueryRow("SELECT 1 FROM accounts WHERE user_id != ? AND (api_token = ? OR user_id = ?) LIMIT 1", userID, token, token).Scan(&exists)
	if err == nil {
		return ErrAPITokenConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec("UPDATE accounts SET api_token = ?, updated_at = datetime('now', 'localtime') WHERE user_id = ?", token, userID); err != nil {
		return err
	}
	return tx.Commit()
}
