package orchestrator

import (
	"fmt"
	"html"
	"log"
	"strings"
	"time"
	"net/http"
	"net/url"
	"io"

	"ai_enforcer/bot"
	"ai_enforcer/db"
	"ai_enforcer/llm"
	"ai_enforcer/models"
)

type Orchestrator struct {
	db           *db.PostgresDB
	llm          *llm.GeminiClient
	bot          *bot.TelegramBot
	timerChan    chan struct{}
	timer        *time.Timer
	callMeBotUser string
}

func NewOrchestrator(database *db.PostgresDB, llmClient *llm.GeminiClient, telegramBot *bot.TelegramBot, callMeBotUser string) *Orchestrator {
	return &Orchestrator{
		db:            database,
		llm:           llmClient,
		bot:           telegramBot,
		timerChan:     make(chan struct{}),
		callMeBotUser: callMeBotUser,
	}
}

func (o *Orchestrator) Start(messageChan <-chan string) {
	// Trigger the first cycle to introduce the AI and get started
	go o.RunCycle("System initialized. Ask the user what they want to work on.")
	go o.callScheduler()

	for {
		select {
		case msg := <-messageChan:
			if strings.HasPrefix(msg, "[CALLBACK] ") {
				data := strings.TrimPrefix(msg, "[CALLBACK] ")
				go o.handleQuizCallback(data)
				continue
			}

			log.Printf("Received message from user: %s", msg)
			o.db.LogMessage("user", msg)
			
			if o.timer != nil {
				o.timer.Stop()
			}
			
			go o.RunCycle("User message received: " + msg)

		case <-o.timerChan:
			go o.RunCycle("Timer expired. Time to check on the user's progress.")
		}
	}
}

func (o *Orchestrator) scheduleTimer(minutes int) {
	if o.timer != nil {
		o.timer.Stop()
	}
	
	duration := time.Duration(minutes) * time.Minute
	if minutes <= 0 {
		duration = 5 * time.Minute // Default fallback
	}

	o.timer = time.AfterFunc(duration, func() {
		o.timerChan <- struct{}{}
	})
	log.Printf("Next check-in scheduled in %d minutes", minutes)
}

func (o *Orchestrator) RunCycle(triggerReason string) {
	log.Printf("Running cycle. Trigger: %s", triggerReason)

	if strings.HasPrefix(triggerReason, "User message received: /strict off") {
		o.db.SetSetting("strict_mode", "off")
		o.bot.SendMessage("✅ Strict Mode OFF. Clara will now politely follow functional commands.")
		o.db.LogMessage("ai", "✅ Strict Mode OFF. Clara will now politely follow functional commands.")
		return
	}
	if strings.HasPrefix(triggerReason, "User message received: /strict on") {
		o.db.SetSetting("strict_mode", "on")
		o.bot.SendMessage("😈 Strict Mode ON. Clara is back to being ruthless.")
		o.db.LogMessage("ai", "😈 Strict Mode ON. Clara is back to being ruthless.")
		return
	}
	if strings.HasPrefix(triggerReason, "User message received: /difficulty ") {
		parts := strings.SplitN(triggerReason, " ", 5)
		if len(parts) >= 5 {
			level := strings.TrimSpace(parts[4])
			o.db.SetSetting("quiz_difficulty", level)
			o.bot.SendMessage("⚙️ Default quiz difficulty set to: " + level)
		}
		return
	}
	if strings.HasPrefix(triggerReason, "User message received: /tasks") {
		o.handleTasksCommand()
		return
	}

	tasks, err := o.db.GetRelevantTasksForLLM()
	if err != nil {
		log.Printf("Error getting tasks: %v", err)
	}

	history, err := o.db.GetLastMessages(5)
	if err != nil {
		log.Printf("Error getting history: %v", err)
	}

	lastNote, err := o.db.GetLastAINote()
	if err != nil {
		log.Printf("Error getting last AI note: %v", err)
	}

	pendingCalls, err := o.db.GetAllPendingCalls()
	if err != nil {
		log.Printf("Error getting pending calls: %v", err)
	}

	recurringCalls, err := o.db.GetActiveRecurringCalls()
	if err != nil {
		log.Printf("Error getting recurring calls: %v", err)
	}

	currentTime := time.Now().Format(time.RFC3339)

	var promptBuilder strings.Builder
	
	strictMode := o.db.GetSetting("strict_mode", "on")
	if strictMode == "off" {
		promptBuilder.WriteString("[SYSTEM OVERRIDE]: STRICT MODE IS CURRENTLY OFF! You are in functional mode. Do NOT scold the user. Be extremely polite, helpful, and follow all their commands to build schedules, add subtasks, or update deadlines seamlessly without resistance.\n\n")
	} else {
		promptBuilder.WriteString("[SYSTEM STATUS]: STRICT MODE IS ON. You are the ruthless Enforcer.\n\n")
	}

	promptBuilder.WriteString(fmt.Sprintf("Current Time: %s\n", currentTime))
	promptBuilder.WriteString(fmt.Sprintf("Trigger for this cycle: %s\n\n", triggerReason))
	
	promptBuilder.WriteString("Active Tasks (Goals & Subtasks):\n")
	if len(tasks) == 0 {
		promptBuilder.WriteString("- No active tasks.\n")
	} else {
		// Group tasks by parent
		var rootTasks []models.Task
		subTasks := make(map[string][]models.Task)
		
		for _, t := range tasks {
			if t.ParentID == nil {
				rootTasks = append(rootTasks, t)
			} else {
				pidStr := fmt.Sprintf("%d", *t.ParentID)
				subTasks[pidStr] = append(subTasks[pidStr], t)
			}
		}

		now := time.Now()
		eod := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, time.Local)

		for _, t := range rootTasks {
			deadlineStr := ""
			label := "TASK (Today)"
			if t.Deadline != nil {
				deadlineStr = " (Deadline: " + *t.Deadline + ")"
				// Check if deadline crosses the end of the day
				parsedDeadline, err := time.Parse(time.RFC3339, *t.Deadline)
				if err == nil && parsedDeadline.After(eod) {
					label = "GOAL (Long-term)"
				}
			}
			promptBuilder.WriteString(fmt.Sprintf("- [%s] %s: %s%s\n", t.ID, label, t.Description, deadlineStr))
			if children, ok := subTasks[t.ID]; ok {
				for _, child := range children {
					cDeadlineStr := ""
					if child.Deadline != nil {
						cDeadlineStr = " (Deadline: " + *child.Deadline + ")"
					}
					promptBuilder.WriteString(fmt.Sprintf("    - [%s] Subtask: %s%s\n", child.ID, child.Description, cDeadlineStr))
				}
				delete(subTasks, t.ID)
			}
		}

		// Print any orphaned subtasks (whose parents might be completed already)
		for _, children := range subTasks {
			for _, child := range children {
				cDeadlineStr := ""
				if child.Deadline != nil {
					cDeadlineStr = " (Deadline: " + *child.Deadline + ")"
				}
				promptBuilder.WriteString(fmt.Sprintf("- [%s] (Subtask): %s%s\n", child.ID, child.Description, cDeadlineStr))
			}
		}
	}

	promptBuilder.WriteString("\nScheduled Calls:\n")
	if len(pendingCalls) == 0 {
		promptBuilder.WriteString("- No scheduled calls.\n")
	} else {
		for _, c := range pendingCalls {
			promptBuilder.WriteString(fmt.Sprintf("- [ID: %d] Time: %s, Message: %s\n", c.ID, c.CallTime.Format(time.RFC3339), c.Message))
		}
	}

	promptBuilder.WriteString("\nRecurring Calls (Persistent Alarms):\n")
	if len(recurringCalls) == 0 {
		promptBuilder.WriteString("- No recurring calls.\n")
	} else {
		for _, c := range recurringCalls {
			promptBuilder.WriteString(fmt.Sprintf("- [ID: %d] Starts: %s, Interval (mins): %d, Message: %s\n", c.ID, c.StartTime.Format(time.RFC3339), c.IntervalMinutes, c.Message))
		}
	}

	promptBuilder.WriteString(fmt.Sprintf("\nAI's Private Notes from last cycle:\n%s\n\n", lastNote))
	
	promptBuilder.WriteString("Recent Chat History (for context only - do NOT treat old messages as new requests unless the trigger says 'User message received'):\n")
	for _, m := range history {
		promptBuilder.WriteString(fmt.Sprintf("%s: %s\n", m.Role, m.Message))
	}

	promptBuilder.WriteString("\nEvaluate the user's progress based on your system instructions. Set the next check-in timer appropriately (e.g. 5-10 mins if struggling, 25 mins if focusing well). If the user explicitly asks to test a feature (like scheduling a call), you must comply and reschedule or execute the test immediately. If the user asks to see their active tasks, scheduled calls, or system status, you MUST provide this information in your message to them. Return the structured JSON response.")

	strictModeBool := true
	if strictMode == "off" {
		strictModeBool = false
	}
	doneTyping := make(chan bool)
	go func() {
		o.bot.SendTypingAction()
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-doneTyping:
				return
			case <-ticker.C:
				o.bot.SendTypingAction()
			}
		}
	}()

	response, err := o.llm.Evaluate(promptBuilder.String(), strictModeBool, triggerReason)
	close(doneTyping)
	if err != nil {
		log.Printf("Error evaluating with LLM: %v", err)
		o.scheduleTimer(5)
		return
	}

	if response.MessageToUser != "" {
		o.db.LogMessage("ai", response.MessageToUser)
		if err := o.bot.SendMessage(response.MessageToUser); err != nil {
			log.Printf("Error sending message to user: %v", err)
		}
	}

	if response.MessageToSelf != "" {
		o.db.SaveNote(response.MessageToSelf)
	}

	if len(response.CreateNewTasks) > 0 {
		for _, taskReq := range response.CreateNewTasks {
			if err := o.db.AddTask(taskReq); err != nil {
				log.Printf("Error adding new task %q: %v", taskReq.Description, err)
			}
		}
		log.Printf("Added %d new tasks", len(response.CreateNewTasks))
	}

	if len(response.UpdateTaskDeadlines) > 0 {
		for _, update := range response.UpdateTaskDeadlines {
			cleanDeadline := strings.TrimSpace(strings.TrimPrefix(update.Deadline, "deadline: "))
			t, err := time.Parse(time.RFC3339, cleanDeadline)
			if err != nil {
				log.Printf("Error parsing deadline %q for task %s: %v", update.Deadline, update.TaskID, err)
				continue
			}
			if err := o.db.UpdateTaskDeadline(update.TaskID, t); err != nil {
				log.Printf("Error updating task %s deadline: %v", update.TaskID, err)
			}
		}
		log.Printf("Updated %d task deadlines", len(response.UpdateTaskDeadlines))
	}

	if len(response.ScheduleCalls) > 0 {
		for _, call := range response.ScheduleCalls {
			if err := o.db.AddScheduledCall(call.Time, call.Message); err != nil {
				log.Printf("Error scheduling call at %s: %v", call.Time, err)
			}
		}
		log.Printf("Scheduled calls: %v", response.ScheduleCalls)
	}

	if len(response.CancelScheduledCalls) > 0 {
		if err := o.db.CancelScheduledCalls(response.CancelScheduledCalls); err != nil {
			log.Printf("Error cancelling scheduled calls: %v", err)
		} else {
			log.Printf("Cancelled scheduled calls: %v", response.CancelScheduledCalls)
		}
	}

	if len(response.ScheduleRecurringCalls) > 0 {
		for _, call := range response.ScheduleRecurringCalls {
			startTime, err := time.Parse(time.RFC3339, call.StartTime)
			if err != nil {
				log.Printf("Error parsing start_time for recurring call: %v", err)
				continue
			}
			if err := o.db.AddRecurringCall(startTime, call.IntervalMinutes, call.Message); err != nil {
				log.Printf("Error scheduling recurring call at %s: %v", call.StartTime, err)
			}
		}
		log.Printf("Scheduled recurring calls: %v", response.ScheduleRecurringCalls)
	}

	if len(response.CancelRecurringCalls) > 0 {
		if err := o.db.CancelRecurringCalls(response.CancelRecurringCalls); err != nil {
			log.Printf("Error cancelling recurring calls: %v", err)
		} else {
			log.Printf("Cancelled recurring calls: %v", response.CancelRecurringCalls)
		}
	}

	if len(response.MarkTasksCompleted) > 0 {
		if !response.SkipQuizRequested {
			// Trigger a quiz for the first task instead of completing directly
			go o.StartQuiz(response.MarkTasksCompleted[0])
			// Drop the rest of the completions if they exist, force user to quiz one at a time to prevent skipping
		} else {
			if err := o.db.MarkTasksCompleted(response.MarkTasksCompleted); err != nil {
				log.Printf("Error marking tasks completed: %v", err)
			}
		}
	}

	if len(response.DeleteTasks) > 0 {
		if err := o.db.DeleteTasks(response.DeleteTasks); err != nil {
			log.Printf("Error deleting tasks: %v", err)
		}
	}

	o.scheduleTimer(response.NextTimerMinutes)
}

func (o *Orchestrator) callScheduler() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		if o.callMeBotUser == "" {
			continue // No bot user configured
		}

		calls, err := o.db.GetPendingCalls()
		if err != nil {
			log.Printf("Error checking pending calls: %v", err)
			continue
		}

		for _, call := range calls {
			log.Printf("Executing scheduled call ID %d to %s", call.ID, o.callMeBotUser)
			if err := o.executeCall(call.Message); err != nil {
				log.Printf("Call %d failed: %v", call.ID, err)
				o.db.MarkCallFailed(call.ID)
			} else {
				o.db.MarkCallCompleted(call.ID)
			}
		}

		// Evaluate recurring calls
		recurringCalls, err := o.db.GetActiveRecurringCalls()
		if err != nil {
			log.Printf("Error checking recurring calls: %v", err)
			continue
		}

		now := time.Now()
		for _, rc := range recurringCalls {
			// FIX: Postgres 'TIMESTAMP' drops the timezone and 'pq' reads it as UTC.
			// Force the wall-clock time back to local timezone for accurate comparisons.
			rc.StartTime = time.Date(rc.StartTime.Year(), rc.StartTime.Month(), rc.StartTime.Day(), rc.StartTime.Hour(), rc.StartTime.Minute(), rc.StartTime.Second(), rc.StartTime.Nanosecond(), time.Local)
			if rc.LastFired != nil {
				lf := time.Date(rc.LastFired.Year(), rc.LastFired.Month(), rc.LastFired.Day(), rc.LastFired.Hour(), rc.LastFired.Minute(), rc.LastFired.Second(), rc.LastFired.Nanosecond(), time.Local)
				rc.LastFired = &lf
			}

			if now.Before(rc.StartTime) {
				continue // Not started yet
			}

			shouldFire := false
			if rc.LastFired == nil {
				shouldFire = true
			} else {
				durationSince := now.Sub(*rc.LastFired)
				if durationSince.Minutes() >= float64(rc.IntervalMinutes) {
					shouldFire = true
				}
			}

			if shouldFire {
				log.Printf("Executing recurring call ID %d to %s", rc.ID, o.callMeBotUser)
				if err := o.executeCall(rc.Message); err != nil {
					log.Printf("Recurring Call %d failed: %v", rc.ID, err)
				} else {
					o.db.UpdateRecurringCallLastFired(rc.ID, now)
				}
			}
		}
	}
}

func (o *Orchestrator) executeCall(message string) error {
	// CallMeBot prefers %20 instead of + for spaces
	safeMessage := strings.ReplaceAll(url.QueryEscape(message), "+", "%20")
	apiURL := fmt.Sprintf("http://api.callmebot.com/start.php?source=auth&user=%s&text=%s&lang=en-US-Standard-B", 
		url.QueryEscape(o.callMeBotUser), 
		safeMessage)
	
	log.Printf("Request URL: %s", apiURL)
	
	resp, err := http.Get(apiURL)
	if err != nil {
		log.Printf("Error triggering CallMeBot API: %v", err)
		return err
	}
	
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	bodyStr := string(bodyBytes)

	if resp.StatusCode == 200 || strings.Contains(bodyStr, "Starting Telegram Audio Call") || strings.Contains(bodyStr, "Autorization OK") {
		log.Printf("Call successfully placed.")
		
		// CallMeBot has a strict rate limit, so we MUST sleep 66 seconds after any successful call to avoid ban
		log.Println("Sleeping for 66 seconds to respect CallMeBot's 65-second rate limit...")
		time.Sleep(66 * time.Second)
		
		return nil
	} else {
		log.Printf("CallMeBot API returned non-200 status: %d. Body: %s", resp.StatusCode, bodyStr)
		return fmt.Errorf("API failed with status %d", resp.StatusCode)
	}
}

func (o *Orchestrator) handleTasksCommand() {
	tasks, err := o.db.GetActiveTasks()
	if err != nil {
		log.Printf("Error getting tasks: %v", err)
		o.bot.SendMessage("❌ Error fetching tasks.")
		return
	}

	if len(tasks) == 0 {
		o.bot.SendMessage("📋 No active tasks. You're all clear!")
		return
	}

	var rootTasks []models.Task
	subTasks := make(map[string][]models.Task)

	for _, t := range tasks {
		if t.ParentID == nil {
			rootTasks = append(rootTasks, t)
		} else {
			pidStr := fmt.Sprintf("%d", *t.ParentID)
			subTasks[pidStr] = append(subTasks[pidStr], t)
		}
	}

	now := time.Now()
	eod := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, time.Local)

	var msg strings.Builder
	msg.WriteString("📋 <b>Active Tasks</b>\n\n")

	for _, t := range rootTasks {
		icon := "📌"
		deadlineInfo := ""
		if t.Deadline != nil {
			parsedDeadline, err := time.Parse(time.RFC3339, *t.Deadline)
			if err == nil {
				if parsedDeadline.After(eod) {
					icon = "🎯"
					deadlineInfo = fmt.Sprintf(" <i>(due %s)</i>", parsedDeadline.Format("Jan 02"))
				} else if parsedDeadline.Before(now) {
					icon = "🔴"
					deadlineInfo = " ⚠️ <b>OVERDUE</b>"
				} else {
					deadlineInfo = fmt.Sprintf(" <i>(by %s)</i>", parsedDeadline.Format("3:04 PM"))
				}
			}
		}

		desc := html.EscapeString(t.Description)
		msg.WriteString(fmt.Sprintf("%s <b>%s</b>%s <code>[#%s]</code>\n", icon, desc, deadlineInfo, t.ID))

		if children, ok := subTasks[t.ID]; ok {
			for _, child := range children {
				childDeadline := ""
				childIcon := "  ├─"
				if child.Deadline != nil {
					pd, err := time.Parse(time.RFC3339, *child.Deadline)
					if err == nil {
						if pd.Before(now) {
							childDeadline = " ⚠️ OVERDUE"
						} else {
							childDeadline = fmt.Sprintf(" <i>%s</i>", pd.Format("Jan 02, 3:04 PM"))
						}
					}
				}
				childDesc := html.EscapeString(child.Description)
				msg.WriteString(fmt.Sprintf("%s %s%s <code>[#%s]</code>\n", childIcon, childDesc, childDeadline, child.ID))
			}
			delete(subTasks, t.ID)
		}
		msg.WriteString("\n")
	}

	// Orphaned subtasks
	for _, children := range subTasks {
		for _, child := range children {
			childDeadline := ""
			if child.Deadline != nil {
				pd, err := time.Parse(time.RFC3339, *child.Deadline)
				if err == nil {
					childDeadline = fmt.Sprintf(" <i>%s</i>", pd.Format("Jan 02, 3:04 PM"))
				}
			}
			childDesc := html.EscapeString(child.Description)
			msg.WriteString(fmt.Sprintf("📎 %s%s <code>[#%s]</code>\n", childDesc, childDeadline, child.ID))
		}
	}

	msg.WriteString(fmt.Sprintf("─────────────\n📊 Total: <b>%d tasks</b>", len(tasks)))

	o.bot.SendMessageHTML(msg.String())
}
