package models

type AIResponse struct {
	MessageToUser      string   `json:"message_to_user"`
	MessageToSelf      string   `json:"message_to_self"`
	CreateNewTasks       []string        `json:"create_new_tasks"`
	MarkTasksCompleted   []string        `json:"mark_tasks_completed"`
	ScheduleCalls        []ScheduledCall `json:"schedule_calls"`
	CancelScheduledCalls []int           `json:"cancel_scheduled_calls"`
	NextTimerMinutes     int             `json:"next_timer_minutes"`
}

type ScheduledCall struct {
	Time    string `json:"time"`    // ISO-8601 formatted time string (e.g. 2026-08-05T08:30:00Z)
	Message string `json:"message"` // The message to be read during the call
}

type Task struct {
	ID          string `json:"id"` 
	Description string `json:"description"`
	Status      string `json:"status"` // "pending", "completed"
}

type ChatMessage struct {
	Role    string `json:"role"`    // "user", "ai"
	Message string `json:"message"`
}
