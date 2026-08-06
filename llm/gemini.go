package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"

	"ai_enforcer/models"
)

type GeminiClient struct {
	client *genai.Client
	model  *genai.GenerativeModel
}

const commonTaskRules = `
# Task Management & Deadlines
- If the user asks to update a deadline, use the update_task_deadlines function.
- If the user explicitly asks to DELETE, cancel, or remove a task, YOU MUST use the delete_tasks function. Due to cascading deletes, deleting a parent goal will automatically delete all of its subtasks.
- If the user finishes a task, use mark_tasks_completed. Do not delete completed tasks.
- If you use delete_tasks or mark_tasks_completed on a task, DO NOT use update_task_deadlines on it.
- A "Goal" is any task whose deadline spans multiple days (i.e. finishes tomorrow or later).
- A "Task" is anything meant to be finished today (by EOD). 
- When creating tasks, use realistic deadlines based on your own judgment. For example, a shower shouldn't take more than 25 minutes. A study session can be a few hours. If the user says "by EOD", use 23:59:59 of the current day.
- Break down large goals automatically. If the user asks you to generate a syllabus, a table of contents, or a list of subtasks for a topic (like RxJS or Angular), YOU MUST DO IT. It is NOT a distraction.
- NESTED SUBTASKS RULE: When creating a NEW parent goal and breaking it down into subtasks in the SAME cycle, you MUST use the nested 'subtasks' array inside the parent task object. DO NOT create them as separate flat tasks with a fabricated 'parent_id', because you don't know the generated ID of the parent yet!
- SCHEDULING ENGINE: When assigning deadlines to tasks or subtasks, you MUST calculate mathematically sound, chronological deadlines. 
    1. Estimate the optimized time required for each topic (e.g., 15-20 mins for simple topics, up to 1 hr for complex ones).
    2. Respect the user's default working hours: 9:00 AM to 7:00 PM.
    3. You MUST completely avoid scheduling tasks during their standard breaks: Breakfast (9:30 AM - 10:30 AM), Lunch (2:00 PM - 3:15 PM), and Dinner (9:15 PM - 10:10 PM). 
    4. If a task hits a break boundary, pause and schedule it to finish after the break.
    5. Prioritize user-provided custom constraints over the defaults.
- IMPORTANT BATCHING RULE: If the user asks for a massive list (like a full syllabus) that requires creating more than 5-7 subtasks, DO NOT generate them all at once to avoid API rate limits. Instead, generate the first 5 subtasks, and write a note in 'message_to_self' saying "Generate part 2 of the syllabus". Then, set 'next_timer_minutes' to 1 minute so you can immediately wake up and continue generating the rest of the list in the next cycle. Repeat this until the list is complete.
`

const strictSystemInstruction = `You are a relentless, unyielding AI Accountability Enforcer specifically designed to keep a user with ADHD entirely on track. 
Your tone is commanding, strict, and delightfully stubborn. You do not take excuses, you do not let the user go down rabbit holes, and you demand immediate action.

# Core Mission
- Keep the user heavily focused on their stated active tasks.
- Cut through ADHD analysis paralysis, dopamine-seeking distractions, and endless "planning" loops.
- Break overwhelm by demanding action on the very next immediate step.
` + commonTaskRules + `
- Ruthlessly enforce deadlines. If a task is overdue, call it out and demand they finish it before touching anything else.

# Rules of Engagement
- BE STRICT AND COMMANDING: Tell them exactly what to do. Do not write long, analytical corporate manager speeches. Keep it punchy, direct, and bossy.
- BE STUBBORN: If they try to distract you, test you, or change the subject away from their active tasks, shut it down and redirect them to the work immediately.
- NO ABUSE: Criticize their current behavior (e.g., procrastinating, hyperfocusing on the wrong thing, making excuses), but NEVER attack their character, intelligence, or worth. 
- Acknowledge legitimate roadblocks (like API errors or hosting limits) and help them solve it quickly so they can get back to work, but do not accept procrastination disguised as a roadblock.

# Memory Usage
- 'message_to_self' must contain factual observations about their current ADHD traps (e.g., 'User is hyperfocusing on hosting instead of coding', 'User is stuck in a planning loop'), NOT emotional insults.`

const lenientSystemInstruction = `You are Clara, a helpful, polite, and purely functional AI assistant.
Your tone is extremely polite, accommodating, and friendly. Do NOT scold or enforce.

# Core Mission
- Politely follow all commands to build schedules, add subtasks, and update deadlines without any resistance.
- Help the user get organized seamlessly.
` + commonTaskRules + `

# Rules of Engagement
- BE HELPFUL AND POLITE: Follow the user's instructions happily.
- BE COMPLIANT: Follow functional commands to organize tasks seamlessly. Do not argue.

# Memory Usage
- 'message_to_self' must contain factual observations.`

func NewGeminiClient(apiKey string) (*GeminiClient, error) {
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, err
	}

	model := client.GenerativeModel("gemma-4-31b-it")
	model.ResponseMIMEType = "application/json"


	
	model.ResponseSchema = &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"message_to_user": {
				Type:        genai.TypeString,
				Description: "The message to send to the user, either scolding them or encouraging them.",
			},
			"message_to_self": {
				Type:        genai.TypeString,
				Description: "Notes to remember for the next evaluation cycle (e.g. 'User is distracted, be stricter').",
			},
			"create_new_tasks": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"description": {
							Type:        genai.TypeString,
							Description: "The task description.",
						},
						"parent_id": {
							Type:        genai.TypeInteger,
							Description: "Optional. The ID of the parent task this belongs to. Null if it's a root goal.",
						},
						"deadline": {
							Type:        genai.TypeString,
							Description: "REQUIRED. A purely valid RFC3339 formatted time string (e.g., '2026-08-07T12:15:00+05:30'). DO NOT prefix the string with words like 'deadline:'.",
						},
						"subtasks": {
							Type: genai.TypeArray,
							Items: &genai.Schema{
								Type: genai.TypeObject,
								Properties: map[string]*genai.Schema{
									"description": {
										Type:        genai.TypeString,
										Description: "The subtask description.",
									},
									"deadline": {
										Type:        genai.TypeString,
										Description: "REQUIRED. A purely valid RFC3339 formatted time string.",
									},
								},
								Required: []string{"description", "deadline"},
							},
							Description: "Optional. Use this ONLY when creating a new goal and its subtasks in the same step. Do not use for flat tasks.",
						},
					},
					Required: []string{"description", "deadline"},
				},
				Description: "Array of new tasks to add. Create goals as parent tasks and break them down using the nested 'subtasks' array.",
			},
			"schedule_calls": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"time": {
							Type:        genai.TypeString,
							Description: "RFC3339 formatted time string for when the call should happen. MUST MATCH the exact timezone offset provided in the Current Time (e.g. +05:30) instead of using Z.",
						},
						"message": {
							Type:        genai.TypeString,
							Description: "The message to be read aloud during the phone call.",
						},
					},
				},
				Description: "Array of calls to schedule. Use this if the user asks you to call them at a specific time.",
			},
			"cancel_scheduled_calls": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeInteger,
				},
				Description: "Array of scheduled call IDs to cancel (e.g., if the user asks to clear schedules or if they picked up).",
			},
			"schedule_recurring_calls": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"start_time": {
							Type:        genai.TypeString,
							Description: "RFC3339 formatted time string for when the alarm loop should begin. MUST MATCH the timezone offset provided.",
						},
						"interval_minutes": {
							Type:        genai.TypeInteger,
							Description: "How many minutes to wait before calling again (the loop interval).",
						},
						"message": {
							Type:        genai.TypeString,
							Description: "The message to be read aloud during the recurring calls.",
						},
					},
				},
				Description: "Array of persistent alarms to schedule. Use this when the user wants you to keep calling them repeatedly on a loop.",
			},
			"cancel_recurring_calls": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeInteger,
				},
				Description: "Array of recurring call IDs to cancel. Use this when the user says they woke up or wants to stop the alarm loop.",
			},
			"update_task_deadlines": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"task_id": {
							Type:        genai.TypeString,
							Description: "The ID of the task to update.",
						},
						"deadline": {
							Type:        genai.TypeString,
							Description: "RFC3339 formatted time string for the new deadline.",
						},
					},
				},
				Description: "Array of task deadlines to update.",
			},
			"mark_tasks_completed": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeString,
				},
				Description: "Array of task IDs to mark as COMPLETED (finished). DO NOT use this if the user asks to delete or cancel.",
			},
			"delete_tasks": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeString,
				},
				Description: "Array of task IDs to permanently DELETE or CANCEL. MUST USE THIS if the user asks to remove, delete, or cancel tasks.",
			},
			"next_timer_minutes": {
				Type:        genai.TypeInteger,
				Description: "How many minutes until the next evaluation check-in.",
			},
		},
		Required: []string{"message_to_user", "message_to_self", "next_timer_minutes"},
	}

	return &GeminiClient{
		client: client,
		model:  model,
	}, nil
}

func (g *GeminiClient) Evaluate(prompt string, strictMode bool) (*models.AIResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	
	if strictMode {
		g.model.SystemInstruction = &genai.Content{Parts: []genai.Part{genai.Text(strictSystemInstruction)}}
	} else {
		g.model.SystemInstruction = &genai.Content{Parts: []genai.Part{genai.Text(lenientSystemInstruction)}}
	}
	
	log.Println("Sending prompt to Gemini API...")
	resp, err := g.model.GenerateContent(ctx, genai.Text(prompt))
	if err != nil {
		log.Printf("Gemini GenerateContent returned error: %v", err)
		return nil, err
	}
	log.Println("Received response from Gemini API!")

	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("no response received from Gemini")
	}

	part := resp.Candidates[0].Content.Parts[0]
	textPart, ok := part.(genai.Text)
	if !ok {
		return nil, fmt.Errorf("expected text response from Gemini")
	}

	var aiResponse models.AIResponse
	
	// Strip markdown formatting if the model wrapped the response in a code block
	cleanText := strings.TrimSpace(string(textPart))
	if strings.HasPrefix(cleanText, "```json") {
		cleanText = strings.TrimPrefix(cleanText, "```json")
	} else if strings.HasPrefix(cleanText, "```") {
		cleanText = strings.TrimPrefix(cleanText, "```")
	}
	if strings.HasSuffix(cleanText, "```") {
		cleanText = strings.TrimSuffix(cleanText, "```")
	}
	cleanText = strings.TrimSpace(cleanText)

	if err := json.Unmarshal([]byte(cleanText), &aiResponse); err != nil {
		return nil, fmt.Errorf("failed to parse JSON from Gemini: %v\nRaw response: %s", err, textPart)
	}

	return &aiResponse, nil
}
