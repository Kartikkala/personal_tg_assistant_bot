package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"ai_enforcer/models"
)

type PostgresDB struct {
	conn *sql.DB
}

func NewPostgresDB(connStr string) (*PostgresDB, error) {
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}

	p := &PostgresDB{conn: db}
	if err := p.initTables(); err != nil {
		return nil, err
	}

	return p, nil
}

func (p *PostgresDB) initTables() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			id SERIAL PRIMARY KEY,
			description TEXT NOT NULL,
			status VARCHAR(50) DEFAULT 'pending',
			parent_id INT REFERENCES tasks(id) ON DELETE CASCADE,
			deadline TIMESTAMPTZ,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS parent_id INT REFERENCES tasks(id) ON DELETE CASCADE`,
		`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS deadline TIMESTAMPTZ`,
		`CREATE TABLE IF NOT EXISTS chat_logs (
			id SERIAL PRIMARY KEY,
			role VARCHAR(50) NOT NULL,
			message TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS ai_notes (
			id SERIAL PRIMARY KEY,
			note TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS scheduled_calls (
			id SERIAL PRIMARY KEY,
			call_time TIMESTAMP NOT NULL,
			message TEXT NOT NULL,
			status VARCHAR(50) DEFAULT 'pending'
		)`,
		`CREATE TABLE IF NOT EXISTS recurring_calls (
			id SERIAL PRIMARY KEY,
			start_time TIMESTAMPTZ NOT NULL,
			interval_minutes INT NOT NULL,
			message TEXT NOT NULL,
			last_fired TIMESTAMPTZ,
			status VARCHAR(50) DEFAULT 'active'
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key VARCHAR(50) PRIMARY KEY,
			value VARCHAR(255) NOT NULL
		)`,
	}

	for _, query := range queries {
		if _, err := p.conn.Exec(query); err != nil {
			return fmt.Errorf("failed to execute query %q: %v", query, err)
		}
	}
	return nil
}

func (p *PostgresDB) GetActiveTasks() ([]models.Task, error) {
	rows, err := p.conn.Query(`SELECT id, description, status, parent_id, deadline FROM tasks WHERE status = 'pending'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []models.Task
	for rows.Next() {
		var t models.Task
		var idInt int
		var parentID sql.NullInt64
		var deadlineTime sql.NullTime

		if err := rows.Scan(&idInt, &t.Description, &t.Status, &parentID, &deadlineTime); err != nil {
			return nil, err
		}
		t.ID = fmt.Sprintf("%d", idInt)
		if parentID.Valid {
			pid := int(parentID.Int64)
			t.ParentID = &pid
		}
		if deadlineTime.Valid {
			// Convert TIMESTAMPTZ to Local and format
			localTime := deadlineTime.Time.Local()
			timeStr := localTime.Format(time.RFC3339)
			t.Deadline = &timeStr
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (p *PostgresDB) GetLastMessages(limit int) ([]models.ChatMessage, error) {
	query := fmt.Sprintf(`SELECT role, message FROM chat_logs ORDER BY created_at DESC LIMIT %d`, limit)
	rows, err := p.conn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []models.ChatMessage
	for rows.Next() {
		var m models.ChatMessage
		if err := rows.Scan(&m.Role, &m.Message); err != nil {
			return nil, err
		}
		messages = append([]models.ChatMessage{m}, messages...) // Prepend so older messages are first
	}
	return messages, nil
}

func (p *PostgresDB) SetSetting(key, value string) error {
	_, err := p.conn.Exec(`
		INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
	`, key, value)
	return err
}

func (p *PostgresDB) GetSetting(key, defaultValue string) string {
	var value string
	err := p.conn.QueryRow(`SELECT value FROM settings WHERE key = $1`, key).Scan(&value)
	if err != nil {
		return defaultValue
	}
	return value
}

func (p *PostgresDB) GetLastAINote() (string, error) {
	var note string
	err := p.conn.QueryRow(`SELECT note FROM ai_notes ORDER BY created_at DESC LIMIT 1`).Scan(&note)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil // No notes yet
		}
		return "", err
	}
	return note, nil
}

func (p *PostgresDB) SaveNote(note string) error {
	_, err := p.conn.Exec(`INSERT INTO ai_notes (note) VALUES ($1)`, note)
	return err
}

func (p *PostgresDB) LogMessage(role, message string) error {
	_, err := p.conn.Exec(`INSERT INTO chat_logs (role, message) VALUES ($1, $2)`, role, message)
	return err
}

func (p *PostgresDB) MarkTasksCompleted(taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	for _, id := range taskIDs {
		_, err := p.conn.Exec(`UPDATE tasks SET status = 'completed' WHERE id = $1`, id)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *PostgresDB) DeleteTasks(taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	for _, id := range taskIDs {
		_, err := p.conn.Exec(`DELETE FROM tasks WHERE id = $1`, id)
		if err != nil {
			return err
		}
	}
	return nil
}

// AddTask is a helper to insert a task easily
func (p *PostgresDB) AddTask(task models.NewTaskRequest) error {
	var deadlineTime *time.Time
	if task.Deadline != nil {
		cleanDeadline := strings.TrimSpace(strings.TrimPrefix(*task.Deadline, "deadline: "))
		t, err := time.Parse(time.RFC3339, cleanDeadline)
		if err != nil {
			return err
		}
		deadlineTime = &t
	}
	
	var insertedID int
	err := p.conn.QueryRow(`INSERT INTO tasks (description, parent_id, deadline) VALUES ($1, $2, $3) RETURNING id`, task.Description, task.ParentID, deadlineTime).Scan(&insertedID)
	if err != nil {
		return err
	}

	for _, subtask := range task.Subtasks {
		subtask.ParentID = &insertedID
		if err := p.AddTask(subtask); err != nil {
			return err
		}
	}
	return nil
}

func (p *PostgresDB) UpdateTaskDeadline(taskID string, deadline time.Time) error {
	_, err := p.conn.Exec(`UPDATE tasks SET deadline = $1 WHERE id = $2`, deadline, taskID)
	return err
}

func (p *PostgresDB) AddScheduledCall(callTime, message string) error {
	t, err := time.Parse(time.RFC3339, callTime)
	if err != nil {
		// Fallback to string insert if parsing fails
		_, err = p.conn.Exec(`INSERT INTO scheduled_calls (call_time, message) VALUES ($1, $2)`, callTime, message)
		return err
	}
	// Use Go's time.Time which the pq driver converts to a safe timestamp
	_, err = p.conn.Exec(`INSERT INTO scheduled_calls (call_time, message) VALUES ($1, $2)`, t, message)
	return err
}

type CallRecord struct {
	ID      int
	Message string
}

func (p *PostgresDB) GetPendingCalls() ([]CallRecord, error) {
	// Pass time.Now() directly to bypass Postgres timezone idiosyncrasies
	rows, err := p.conn.Query(`SELECT id, message FROM scheduled_calls WHERE status = 'pending' AND call_time <= $1`, time.Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var calls []CallRecord
	for rows.Next() {
		var c CallRecord
		if err := rows.Scan(&c.ID, &c.Message); err != nil {
			return nil, err
		}
		calls = append(calls, c)
	}
	return calls, nil
}

func (p *PostgresDB) MarkCallCompleted(id int) error {
	_, err := p.conn.Exec(`UPDATE scheduled_calls SET status = 'completed' WHERE id = $1`, id)
	return err
}

func (p *PostgresDB) MarkCallFailed(id int) error {
	_, err := p.conn.Exec(`UPDATE scheduled_calls SET status = 'failed' WHERE id = $1`, id)
	return err
}

func (p *PostgresDB) CancelScheduledCalls(callIDs []int) error {
	if len(callIDs) == 0 {
		return nil
	}
	for _, id := range callIDs {
		_, err := p.conn.Exec(`UPDATE scheduled_calls SET status = 'cancelled' WHERE id = $1`, id)
		if err != nil {
			return err
		}
	}
	return nil
}

type ScheduledCallRecord struct {
	ID       int
	CallTime time.Time
	Message  string
}

func (p *PostgresDB) GetAllPendingCalls() ([]ScheduledCallRecord, error) {
	rows, err := p.conn.Query(`SELECT id, call_time, message FROM scheduled_calls WHERE status = 'pending' ORDER BY call_time ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var calls []ScheduledCallRecord
	for rows.Next() {
		var c ScheduledCallRecord
		if err := rows.Scan(&c.ID, &c.CallTime, &c.Message); err != nil {
			return nil, err
		}
		calls = append(calls, c)
	}
	return calls, nil
}

func (p *PostgresDB) AddRecurringCall(startTime time.Time, intervalMinutes int, message string) error {
	_, err := p.conn.Exec(`INSERT INTO recurring_calls (start_time, interval_minutes, message) VALUES ($1, $2, $3)`, startTime, intervalMinutes, message)
	return err
}

type RecurringCallRecord struct {
	ID              int
	StartTime       time.Time
	IntervalMinutes int
	Message         string
	LastFired       *time.Time
}

func (p *PostgresDB) GetActiveRecurringCalls() ([]RecurringCallRecord, error) {
	rows, err := p.conn.Query(`SELECT id, start_time, interval_minutes, message, last_fired FROM recurring_calls WHERE status = 'active' ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var calls []RecurringCallRecord
	for rows.Next() {
		var c RecurringCallRecord
		if err := rows.Scan(&c.ID, &c.StartTime, &c.IntervalMinutes, &c.Message, &c.LastFired); err != nil {
			return nil, err
		}
		calls = append(calls, c)
	}
	return calls, nil
}

func (p *PostgresDB) UpdateRecurringCallLastFired(id int, lastFired time.Time) error {
	_, err := p.conn.Exec(`UPDATE recurring_calls SET last_fired = $1 WHERE id = $2`, lastFired, id)
	return err
}

func (p *PostgresDB) CancelRecurringCalls(callIDs []int) error {
	if len(callIDs) == 0 {
		return nil
	}
	for _, id := range callIDs {
		_, err := p.conn.Exec(`UPDATE recurring_calls SET status = 'cancelled' WHERE id = $1`, id)
		if err != nil {
			return err
		}
	}
	return nil
}
