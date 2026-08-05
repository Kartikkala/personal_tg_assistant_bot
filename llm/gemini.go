package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"github.com/google/generative-ai-go/genai"
	"google.golang.org/api/option"

	"ai_enforcer/models"
)

type GeminiClient struct {
	client *genai.Client
	model  *genai.GenerativeModel
}

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
					Type: genai.TypeString,
				},
				Description: "Array of new task descriptions to add to the user's active task list if they mention new tasks.",
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
			"mark_tasks_completed": {
				Type: genai.TypeArray,
				Items: &genai.Schema{
					Type: genai.TypeString,
				},
				Description: "Array of task IDs to mark as completed based on user input.",
			},
			"next_timer_minutes": {
				Type:        genai.TypeInteger,
				Description: "How many minutes until the next evaluation check-in.",
			},
		},
		Required: []string{"message_to_user", "message_to_self", "create_new_tasks", "schedule_calls", "mark_tasks_completed", "next_timer_minutes"},
	}

	return &GeminiClient{
		client: client,
		model:  model,
	}, nil
}

func (g *GeminiClient) Evaluate(prompt string) (*models.AIResponse, error) {
	ctx := context.Background()
	
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
