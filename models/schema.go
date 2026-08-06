package models

type AIResponse struct {
	MessageToUser          string               `json:"message_to_user"`
	MessageToSelf          string               `json:"message_to_self"`
	CreateNewTasks         []NewTaskRequest     `json:"create_new_tasks"`
	MarkTasksCompleted     []string             `json:"mark_tasks_completed"`
	DeleteTasks            []string             `json:"delete_tasks"`
	ScheduleCalls          []ScheduledCall      `json:"schedule_calls"`
	CancelScheduledCalls   []int                `json:"cancel_scheduled_calls"`
	ScheduleRecurringCalls []RecurringCall      `json:"schedule_recurring_calls"`
	CancelRecurringCalls   []int                `json:"cancel_recurring_calls"`
	UpdateTaskDeadlines    []UpdateTaskDeadline `json:"update_task_deadlines"`
	NextTimerMinutes       int                  `json:"next_timer_minutes"`
}

type UpdateTaskDeadline struct {
	TaskID   string `json:"task_id"`
	Deadline string `json:"deadline"`
}

type NewTaskRequest struct {
	Description string           `json:"description"`
	ParentID    *int             `json:"parent_id,omitempty"` // null if it's a root goal
	Deadline    *string          `json:"deadline,omitempty"`  // RFC3339 formatted time string
	Subtasks    []NewTaskRequest `json:"subtasks,omitempty"`  // Nested subtasks created at the same time
}

type ScheduledCall struct {
	Time    string `json:"time"`    // ISO-8601 formatted time string (e.g. 2026-08-05T08:30:00Z)
	Message string `json:"message"` // The message to be read during the call
}

type RecurringCall struct {
	StartTime       string `json:"start_time"`
	IntervalMinutes int    `json:"interval_minutes"`
	Message         string `json:"message"`
}

type Task struct {
	ID          string  `json:"id"`
	Description string  `json:"description"`
	Status      string  `json:"status"` // "pending", "completed"
	ParentID    *int    `json:"parent_id"`
	Deadline    *string `json:"deadline"`
}

type ChatMessage struct {
	Role    string `json:"role"`    // "user", "ai"
	Message string `json:"message"`
}
