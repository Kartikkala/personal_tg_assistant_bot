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

type roadmapModelEntry struct {
	model *genai.GenerativeModel
	name  string
}

type GeminiClient struct {
	client        *genai.Client
	defaultModel  *genai.GenerativeModel
	roadmapModels []roadmapModelEntry
}

const commonTaskRules = `
# Task Management & Deadlines
- If the user asks to recalculate, reschedule, or update a deadline, YOU MUST use the update_task_deadlines function to reflect the changes in the database. Do not just output a new schedule in text without actually calling the function!
- If the user explicitly asks to DELETE, cancel, or remove a task, YOU MUST use the delete_tasks function. Due to cascading deletes, deleting a parent goal will automatically delete all of its subtasks.
- QUIZ VALIDATION RULE: By default, when a user finishes a task, the system will generate a Quiz to validate their knowledge. If the user says they finished a task, YOU MUST include it in 'mark_tasks_completed'. 
- IF the user explicitly asks to start a quiz on a topic WITHOUT marking it completed, you MUST use the 'start_quizzes' array and provide the task ID. Do not use 'mark_tasks_completed' for this.
- IF the user explicitly says they want to skip the quiz, bypass the quiz, or that they already know the topic well, you MUST set 'skip_quiz_requested' to true.
- VERY IMPORTANT: If you are launching a quiz (via start_quizzes OR because skip_quiz_requested is false during completion), DO NOT say "I've marked the task as completed" in your 'message_to_user'. Tell the user you are launching a validation quiz.
- If you use delete_tasks or mark_tasks_completed on a task, DO NOT use update_task_deadlines on it.
- A "Goal" is any task whose deadline spans multiple days (i.e. finishes tomorrow or later).
- A "Task" is anything meant to be finished today (by EOD). 
- When creating tasks, use realistic deadlines based on your own judgment. For example, a shower shouldn't take more than 25 minutes. A study session can be a few hours. If the user says "by EOD", use 23:59:59 of the current day.
- Break down large goals automatically. If the user asks you to generate a syllabus, a table of contents, or a list of subtasks for a topic (like RxJS or Angular), YOU MUST DO IT. It is NOT a distraction.
- NESTED SUBTASKS RULE: When creating a NEW parent goal and breaking it down into subtasks in the SAME cycle, you MUST use the nested 'subtasks' array inside the parent task object. DO NOT create them as separate flat tasks with a fabricated 'parent_id', because you don't know the generated ID of the parent yet!
- SCHEDULING ENGINE: When assigning or recalculating deadlines, you MUST calculate mathematically sound, chronological deadlines. 
    1. Deadlines are COMPLETION TIMES, not start times! If the user starts working at 9:30 AM on a 1-hour task, the deadline is 10:30 AM (not 9:30 AM).
    2. Estimate the optimized time required for each topic (e.g., 15-20 mins for simple topics, up to 1 hr for complex ones) and add that duration to the PREVIOUS task's deadline to get the new deadline.
    3. Respect the user's default working hours: 9:00 AM to 7:00 PM.
    4. You MUST completely avoid scheduling tasks during their standard breaks: Breakfast (9:30 AM - 10:30 AM), Lunch (2:00 PM - 3:15 PM), and Dinner (9:15 PM - 10:10 PM). 
    5. If a calculated deadline overlaps a break boundary, push the deadline to finish after the break.
    6. Prioritize user-provided custom constraints over the defaults.
- SINGLE-PASS ROADMAP RULE: If the user uses the /goal command or asks for a syllabus/roadmap, you MUST generate an EXHAUSTIVE, chapter-by-chapter breakdown using the 'create_new_tasks' JSON array. Follow these rules STRICTLY:
    1. DO NOT summarize the roadmap in 'message_to_user'. The roadmap lives ENTIRELY in the 'create_new_tasks' array as structured data. Your 'message_to_user' should only be a short confirmation like 'Done! Your roadmap is ready. Use /tasks to view it.'
    2. DO NOT create '5 phases' or '6 modules'. Instead, create 20-30+ individual parent tasks, one for each CHAPTER or TOPIC AREA (e.g., 'Chapter 1: Python Variables & Data Types', 'Chapter 2: Control Flow', 'Chapter 3: Functions & Scope', etc).
    3. Each chapter (parent task) MUST have 5-10 granular subtasks inside its 'subtasks' array. Each subtask should be a specific, actionable lesson (e.g., 'Practice list comprehensions with 5 exercises', 'Build a simple CSV parser using pandas').
    4. The total number of tasks+subtasks should be 100-300+ for a multi-week roadmap. You have a 65k+ token output limit. USE IT.
    5. Every single subtask MUST have its own individual deadline calculated using the SCHEDULING ENGINE rules.
- LENIENT TIME-BLOCKING: When estimating time for topics in a roadmap, be highly lenient and intelligent. Give more than enough time for each topic so the user isn't rushed. Account for all work hours and breaks strictly.
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

	defaultModel := client.GenerativeModel("gemma-4-31b-it")
	defaultModel.ResponseMIMEType = "application/json"

	schema := &genai.Schema{
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
							Description: "REQUIRED. A purely valid RFC3339 formatted time string for the new deadline. DO NOT prefix with 'deadline:'.",
						},
					},
					Required: []string{"task_id", "deadline"},
				},
				Description: "Array of task deadlines to update. MUST use this if you agree to recalculate or reschedule tasks.",
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
			"skip_quiz_requested": {
				Type:        genai.TypeBoolean,
				Description: "Set to true if the user explicitly asked to skip, bypass, or opt-out of the quiz when finishing a task.",
			},
			"start_quizzes": {
				Type: genai.TypeArray,
				Items: &genai.Schema{Type: genai.TypeString},
				Description: "Array of task IDs to explicitly launch a quiz for. Use this if the user asks to be quizzed on a specific topic or wants to start a quiz without marking a task as completed. You can also provide a freeform string like 'Python Syntax' if the user asks for an ad-hoc quiz not in their tasks.",
			},
		},
		Required: []string{"message_to_user", "message_to_self", "next_timer_minutes"},
	}

	roadmapModelNames := []string{"gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash"}
	var roadmapModels []roadmapModelEntry
	for _, name := range roadmapModelNames {
		m := client.GenerativeModel(name)
		m.ResponseMIMEType = "application/json"
		m.ResponseSchema = schema
		roadmapModels = append(roadmapModels, roadmapModelEntry{model: m, name: name})
	}

	defaultModel.ResponseSchema = schema

	return &GeminiClient{
		client:        client,
		defaultModel:  defaultModel,
		roadmapModels: roadmapModels,
	}, nil
}

func (g *GeminiClient) Evaluate(prompt string, strictMode bool, triggerReason string) (*models.AIResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	
	triggerLower := strings.ToLower(triggerReason)
	isRoadmap := strings.Contains(triggerLower, "/goal") || strings.Contains(triggerLower, "roadmap") || strings.Contains(triggerLower, "syllabus")

	var modelCandidates []roadmapModelEntry
	if isRoadmap {
		modelCandidates = g.roadmapModels
	} else {
		modelCandidates = []roadmapModelEntry{{model: g.defaultModel, name: "gemma-4-31b-it"}}
	}

	var resp *genai.GenerateContentResponse
	var err error

	recitationRetries := 0
	for i := 0; i < len(modelCandidates); i++ {
		entry := modelCandidates[i]
		if strictMode {
			entry.model.SystemInstruction = &genai.Content{Parts: []genai.Part{genai.Text(strictSystemInstruction)}}
		} else {
			entry.model.SystemInstruction = &genai.Content{Parts: []genai.Part{genai.Text(lenientSystemInstruction)}}
		}

		log.Printf("Sending prompt to %s...", entry.name)
		resp, err = entry.model.GenerateContent(ctx, genai.Text(prompt))
		if err == nil {
			break
		}

		errStr := err.Error()
		
		// Recitation errors are transient — retry the same model up to 2 times
		if strings.Contains(errStr, "Recitation") && recitationRetries < 2 {
			recitationRetries++
			log.Printf("Recitation filter triggered on %s. Retrying (%d/2)...", entry.name, recitationRetries)
			time.Sleep(3 * time.Second)
			i-- // retry same model index
			continue
		}

		if strings.Contains(errStr, "429") && i < len(modelCandidates)-1 {
			log.Printf("Rate limited (429) on %s. Falling back to next model...", entry.name)
			continue
		}

		log.Printf("Gemini GenerateContent returned error on %s: %v", entry.name, err)
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
	cleanText = strings.TrimSuffix(cleanText, "```")
	cleanText = strings.TrimSpace(cleanText)

	if err := json.Unmarshal([]byte(cleanText), &aiResponse); err != nil {
		return nil, fmt.Errorf("failed to parse AI response JSON: %v\nRaw response: %s", err, cleanText)
	}

	return &aiResponse, nil
}

func (g *GeminiClient) GenerateQuiz(ctx context.Context, task *models.Task, difficulty string, isGoal bool) (*models.QuizGenerationResponse, error) {
	var modelCandidates []string
	if isGoal {
		for _, m := range g.roadmapModels {
			modelCandidates = append(modelCandidates, m.name)
		}
		modelCandidates = append(modelCandidates, "gemma-4-31b-it")
	} else {
		modelCandidates = []string{"gemma-4-31b-it", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash"}
	}

	schema := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"questions": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"question":      {Type: genai.TypeString, Description: "The question text, markdown formatting allowed"},
						"options":       {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}, Description: "Exactly 4 options"},
						"correct_index": {Type: genai.TypeInteger, Description: "0-based index of the correct option (0-3)"},
						"explanation":   {Type: genai.TypeString, Description: "Brief explanation of why the correct answer is right"},
						"is_coding":     {Type: genai.TypeBoolean, Description: "Set to true if this is a coding/scenario question"},
					},
					Required: []string{"question", "options", "correct_index", "explanation", "is_coding"},
				},
			},
		},
		Required: []string{"questions"},
	}

	prompt := fmt.Sprintf("Generate a quiz for the topic: '%s'. Difficulty level: %s. Is this a major Goal? %v. If it is a goal, generate up to 20 questions including at least 1 coding scenario. If it is a subtask, generate 3 to 10 questions.", task.Description, difficulty, isGoal)

	var resp *genai.GenerateContentResponse
	var err error

	for _, name := range modelCandidates {
		tempModel := g.client.GenerativeModel(name)
		tempModel.ResponseMIMEType = "application/json"
		tempModel.ResponseSchema = schema

		resp, err = tempModel.GenerateContent(ctx, genai.Text(prompt))
		if err == nil {
			break
		}
		log.Printf("GenerateQuiz Model %s failed: %v", name, err)
	}

	if err != nil {
		return nil, fmt.Errorf("all models failed to generate quiz. last error: %v", err)
	}

	part := resp.Candidates[0].Content.Parts[0]
	textPart, ok := part.(genai.Text)
	if !ok {
		return nil, fmt.Errorf("expected text response from Gemini")
	}

	cleanText := strings.TrimSpace(string(textPart))
	if strings.HasPrefix(cleanText, "```json") {
		cleanText = strings.TrimPrefix(cleanText, "```json")
	} else if strings.HasPrefix(cleanText, "```") {
		cleanText = strings.TrimPrefix(cleanText, "```")
	}
	cleanText = strings.TrimSuffix(cleanText, "```")
	cleanText = strings.TrimSpace(cleanText)

	var quizResp models.QuizGenerationResponse
	if err := json.Unmarshal([]byte(cleanText), &quizResp); err != nil {
		return nil, fmt.Errorf("failed to parse AI response JSON: %v\nRaw response: %s", err, cleanText)
	}
	return &quizResp, nil
}

func (g *GeminiClient) AnalyzeQuiz(ctx context.Context, session *models.QuizSession, difficulty string) (*models.QuizAnalysisResponse, error) {

	
	schema := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"message_to_user": {Type: genai.TypeString, Description: "A message evaluating their performance."},
			"passed": {Type: genai.TypeBoolean, Description: "True if they passed the quiz, false otherwise."},
			"create_new_tasks": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeObject,
					Properties: map[string]*genai.Schema{
						"description": {Type: genai.TypeString},
						"deadline": {Type: genai.TypeString},
					},
					Required: []string{"description", "deadline"},
				},
				Description: "Array of new tasks to re-add failed subtopics. Set deadline appropriately.",
			},
		},
		Required: []string{"message_to_user", "passed"},
	}

	modelCandidates := []string{"gemma-4-31b-it", "gemini-3.7-flash", "gemini-3.6-flash", "gemini-3.5-flash"}
	prompt := fmt.Sprintf("Analyze quiz performance for task: '%s'. Score: %d / %d. Difficulty was: %s. Determine if they passed. If they failed, generate new tasks for the topics they failed.", session.TaskID, session.CorrectAnswers, len(session.Questions), difficulty)

	var resp *genai.GenerateContentResponse
	var err error

	for _, name := range modelCandidates {
		tempModel := g.client.GenerativeModel(name)
		tempModel.ResponseMIMEType = "application/json"
		tempModel.ResponseSchema = schema

		resp, err = tempModel.GenerateContent(ctx, genai.Text(prompt))
		if err == nil {
			break
		}
		log.Printf("AnalyzeQuiz Model %s failed: %v", name, err)
	}

	if err != nil {
		return nil, fmt.Errorf("all models failed to analyze quiz. last error: %v", err)
	}

	part := resp.Candidates[0].Content.Parts[0]
	textPart, ok := part.(genai.Text)
	if !ok {
		return nil, fmt.Errorf("expected text")
	}

	cleanText := strings.TrimSpace(string(textPart))
	if strings.HasPrefix(cleanText, "```json") {
		cleanText = strings.TrimPrefix(cleanText, "```json")
	} else if strings.HasPrefix(cleanText, "```") {
		cleanText = strings.TrimPrefix(cleanText, "```")
	}
	cleanText = strings.TrimSuffix(cleanText, "```")
	cleanText = strings.TrimSpace(cleanText)

	var analysis models.QuizAnalysisResponse
	if err := json.Unmarshal([]byte(cleanText), &analysis); err != nil {
		return nil, fmt.Errorf("failed to parse AI response JSON: %v\nRaw response: %s", err, cleanText)
	}
	return &analysis, nil
}
