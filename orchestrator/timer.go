package orchestrator

import (
	"fmt"
	"log"
	"strings"
	"time"
	"net/http"
	"net/url"
	"io"

	"ai_enforcer/bot"
	"ai_enforcer/db"
	"ai_enforcer/llm"
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

	tasks, err := o.db.GetActiveTasks()
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
	promptBuilder.WriteString(fmt.Sprintf("Current Time: %s\n", currentTime))
	promptBuilder.WriteString(fmt.Sprintf("Trigger for this cycle: %s\n\n", triggerReason))
	
	promptBuilder.WriteString("Active Tasks:\n")
	if len(tasks) == 0 {
		promptBuilder.WriteString("- No active tasks.\n")
	} else {
		for _, t := range tasks {
			promptBuilder.WriteString(fmt.Sprintf("- [%s] %s\n", t.ID, t.Description))
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
	
	promptBuilder.WriteString("Recent Chat History:\n")
	for _, m := range history {
		promptBuilder.WriteString(fmt.Sprintf("%s: %s\n", m.Role, m.Message))
	}

	promptBuilder.WriteString("\nEvaluate the user's progress based on your system instructions. Set the next check-in timer appropriately (e.g. 5-10 mins if struggling, 25 mins if focusing well). If the user explicitly asks to test a feature (like scheduling a call), you must comply and reschedule or execute the test immediately. If the user asks to see their active tasks, scheduled calls, or system status, you MUST provide this information in your message to them. Return the structured JSON response.")

	response, err := o.llm.Evaluate(promptBuilder.String())
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
		for _, taskDesc := range response.CreateNewTasks {
			if err := o.db.AddTask(taskDesc); err != nil {
				log.Printf("Error adding new task %q: %v", taskDesc, err)
			}
		}
		log.Printf("Added new tasks: %v", response.CreateNewTasks)
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
		o.db.MarkTasksCompleted(response.MarkTasksCompleted)
		log.Printf("Marked tasks completed: %v", response.MarkTasksCompleted)
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
