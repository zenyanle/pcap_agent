package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"pcap_agent/internal/common"
	"time"

	_ "modernc.org/sqlite"
)

// Store handles SQLite persistence for sessions, rounds, and steps.
type Store struct {
	db *sql.DB
}

// OpenStore opens (or creates) a SQLite database at the given path and runs migrations.
func OpenStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	ddl := `
	CREATE TABLE IF NOT EXISTS pcap_files (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		file_name   TEXT NOT NULL,
		file_path   TEXT NOT NULL,
		file_size   INTEGER DEFAULT 0,
		file_hash   TEXT DEFAULT '',
		created_at  TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE IF NOT EXISTS sessions (
		id          TEXT PRIMARY KEY,
		pcap_path   TEXT NOT NULL,
		pcap_file_id INTEGER REFERENCES pcap_files(id),
		created_at  TEXT NOT NULL DEFAULT (datetime('now')),
		updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE IF NOT EXISTS rounds (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id  TEXT NOT NULL REFERENCES sessions(id),
		round_num   INTEGER NOT NULL,
		user_query  TEXT NOT NULL,
		plan_json   TEXT DEFAULT '',
		table_schema TEXT DEFAULT '',
		report      TEXT DEFAULT '',
		findings    TEXT DEFAULT '',
		operation_log TEXT DEFAULT '',
		created_at  TEXT NOT NULL DEFAULT (datetime('now'))
	);
	CREATE TABLE IF NOT EXISTS steps (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		round_id    INTEGER NOT NULL REFERENCES rounds(id),
		step_id     INTEGER NOT NULL,
		intent      TEXT NOT NULL,
		findings    TEXT DEFAULT '',
		actions     TEXT DEFAULT '',
		status      TEXT DEFAULT 'pending',
		created_at  TEXT NOT NULL DEFAULT (datetime('now'))
	);`
	_, err := s.db.Exec(ddl)
	return err
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// SavePcapFile records a PCAP file for future reference.
func (s *Store) SavePcapFile(fileName, filePath string, fileSize int64, fileHash string) (int64, error) {
	res, err := s.db.Exec(
		"INSERT INTO pcap_files (file_name, file_path, file_size, file_hash) VALUES (?, ?, ?, ?)",
		fileName, filePath, fileSize, fileHash,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CreateSession creates a new session record.
func (s *Store) CreateSession(id, pcapPath string) error {
	_, err := s.db.Exec(
		"INSERT INTO sessions (id, pcap_path) VALUES (?, ?)",
		id, pcapPath,
	)
	return err
}

// TouchSession updates the session's updated_at timestamp.
func (s *Store) TouchSession(id string) error {
	_, err := s.db.Exec(
		"UPDATE sessions SET updated_at = ? WHERE id = ?",
		time.Now().UTC().Format(time.RFC3339), id,
	)
	return err
}

// SessionExists checks if a session with the given ID exists.
func (s *Store) SessionExists(id string) (bool, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE id = ?", id).Scan(&count)
	return count > 0, err
}

// GetSessionPcapPath returns the PCAP path for a session.
func (s *Store) GetSessionPcapPath(id string) (string, error) {
	var path string
	err := s.db.QueryRow("SELECT pcap_path FROM sessions WHERE id = ?", id).Scan(&path)
	return path, err
}

// SaveRound saves a completed round of planner-executor pipeline.
func (s *Store) SaveRound(sessionID string, roundNum int, userQuery string, plan common.Plan, report, findings, opLog string) (int64, error) {
	planJSON, _ := json.Marshal(plan)
	res, err := s.db.Exec(
		"INSERT INTO rounds (session_id, round_num, user_query, plan_json, table_schema, report, findings, operation_log) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		sessionID, roundNum, userQuery, string(planJSON), plan.TableSchema, report, findings, opLog,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetRoundCount returns the number of rounds in a session.
func (s *Store) GetRoundCount(sessionID string) (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM rounds WHERE session_id = ?", sessionID).Scan(&count)
	return count, err
}

// SaveStep saves a completed step within a round.
func (s *Store) SaveStep(roundID int64, stepID int, intent, findings, actions, status string) error {
	_, err := s.db.Exec(
		"INSERT INTO steps (round_id, step_id, intent, findings, actions, status) VALUES (?, ?, ?, ?, ?, ?)",
		roundID, stepID, intent, findings, actions, status,
	)
	return err
}

// GetSessionHistory loads accumulated context from all previous rounds.
func (s *Store) GetSessionHistory(sessionID string) (*common.SessionHistory, error) {
	rows, err := s.db.Query(
		"SELECT round_num, findings, operation_log, report FROM rounds WHERE session_id = ? ORDER BY round_num ASC",
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	history := &common.SessionHistory{}
	var allFindings []string
	var allOpLogs []string

	for rows.Next() {
		var roundNum int
		var findings, opLog, report string
		if err := rows.Scan(&roundNum, &findings, &opLog, &report); err != nil {
			return nil, err
		}
		if findings != "" {
			allFindings = append(allFindings, fmt.Sprintf("## Round %d\n%s", roundNum, findings))
		}
		if opLog != "" {
			allOpLogs = append(allOpLogs, fmt.Sprintf("## Round %d\n%s", roundNum, opLog))
		}
		if report != "" {
			history.AllReports = append(history.AllReports, report)
			history.PreviousReport = report
		}
	}

	if len(allFindings) > 0 {
		result := ""
		for _, f := range allFindings {
			result += f + "\n\n"
		}
		history.Findings = result
	}
	if len(allOpLogs) > 0 {
		result := ""
		for _, l := range allOpLogs {
			result += l + "\n\n"
		}
		history.OperationLog = result
	}

	return history, rows.Err()
}
