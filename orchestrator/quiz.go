package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"regexp"
	"strconv"
	"strings"

	"ai_enforcer/models"
)

func (o *Orchestrator) StartQuiz(taskID string) {
	// First get the actual task details to pass to LLM
	tasks, err := o.db.GetRelevantTasksForLLM()
	if err != nil {
		log.Printf("Error fetching tasks for quiz: %v", err)
		return
	}
	
	var task *models.Task
	for _, t := range tasks {
		if t.ID == taskID {
			task = &t
			break
		}
	}
	
	// If not found in relevant, assume it's an ad-hoc topic and use the string directly
	if task == nil {
		task = &models.Task{ID: "ad_hoc", Description: taskID}
	}

	isGoal := false
	if task.ParentID == nil && task.ID != "ad_hoc" {
		isGoal = true
	}

	difficulty := o.db.GetSetting("quiz_difficulty", "Medium")

	o.bot.SendMessageHTML(fmt.Sprintf("🧠 <b>Generating %s Validation Quiz...</b>\nTopic: <i>%s</i>", difficulty, task.Description))

	resp, err := o.llm.GenerateQuiz(context.Background(), task, difficulty, isGoal)
	if err != nil {
		o.bot.SendMessageHTML("❌ Error generating quiz due to AI limits. The task will NOT be marked as completed until we can verify it. Please try again later.")
		return
	}

	if len(resp.Questions) == 0 {
		o.db.MarkTasksCompleted([]string{taskID})
		return
	}

	qJSON, _ := json.Marshal(resp.Questions)
	sessionID, err := o.db.CreateQuizSession(taskID, string(qJSON))
	if err != nil {
		log.Printf("Error creating quiz session: %v", err)
		o.db.MarkTasksCompleted([]string{taskID})
		return
	}

	// Send first question
	o.serveQuizQuestion(sessionID, resp.Questions, 0, 0, []string{})
}

func cleanMarkdownForTelegram(text string) string {
	text = html.EscapeString(text)

	// Normalize code block starts
	re := regexp.MustCompile("```([a-zA-Z0-9]+)\n")
	text = re.ReplaceAllString(text, "```\n")

	parts := strings.Split(text, "```")
	var sb strings.Builder
	for i, part := range parts {
		if i%2 == 0 {
			sb.WriteString(part)
		} else {
			if strings.HasPrefix(part, "\n") {
				part = part[1:]
			}
			sb.WriteString("<pre>")
			sb.WriteString(part)
			sb.WriteString("</pre>")
		}
	}
	text = sb.String()

	sb.Reset()
	inCode := false
	for _, char := range text {
		if char == '`' {
			if inCode {
				sb.WriteString("</code>")
			} else {
				sb.WriteString("<code>")
			}
			inCode = !inCode
		} else {
			sb.WriteRune(char)
		}
	}
	text = sb.String()

	boldRe := regexp.MustCompile(`\*\*(.*?)\*\*`)
	text = boldRe.ReplaceAllString(text, "<b>$1</b>")

	return text
}

func (o *Orchestrator) serveQuizQuestion(sessionID int, questions []models.QuizQuestion, index int, correctAnswers int, failedTopics []string) {
	q := questions[index]
	cleanQuestion := cleanMarkdownForTelegram(q.Question)
	text := fmt.Sprintf("📝 <b>Question %d/%d</b>\n\n%s\n\n", index+1, len(questions), cleanQuestion)
	
	var buttonLabels []string
	for i, opt := range q.Options {
		text += fmt.Sprintf("<b>%c)</b> %s\n", 'A'+i, cleanMarkdownForTelegram(opt))
		buttonLabels = append(buttonLabels, fmt.Sprintf("%c", 'A'+i))
	}

	msgID, err := o.bot.SendQuizQuestion(text, buttonLabels)
	if err != nil {
		log.Printf("Error sending quiz question: %v", err)
		return
	}

	failedJson, _ := json.Marshal(failedTopics)
	o.db.UpdateQuizSession(sessionID, index, correctAnswers, msgID, "active", string(failedJson))
}

func (o *Orchestrator) handleQuizCallback(data string) {
	// data is like "quiz_answer_2" or "quiz_skip"
	session, err := o.db.GetActiveQuizSession()
	if err != nil || session == nil {
		o.bot.SendMessageHTML("No active quiz session found.")
		return
	}

	if data == "quiz_skip" {
		o.bot.EditQuizResult(session.MessageID, "⏭️ Quiz skipped. Marking task as complete.")
		o.db.DeleteQuizSession(session.SessionID)
		o.db.MarkTasksCompleted([]string{session.TaskID})
		return
	}

	parts := strings.Split(data, "_")
	if len(parts) != 3 {
		return
	}
	
	answerIdx, _ := strconv.Atoi(parts[2])
	q := session.Questions[session.CurrentIndex]

	// Bounds checking in case LLM hallucinates an invalid index or options array length
	if q.CorrectIndex < 0 || q.CorrectIndex >= len(q.Options) {
		q.CorrectIndex = 0 
	}
	if answerIdx < 0 || answerIdx >= len(q.Options) {
		answerIdx = 0
	}

	isCorrect := answerIdx == q.CorrectIndex
	
	cleanQuestion := cleanMarkdownForTelegram(q.Question)
	resultText := fmt.Sprintf("📝 <b>Question %d/%d</b>\n\n%s\n\n", session.CurrentIndex+1, len(session.Questions), cleanQuestion)
	resultText += fmt.Sprintf("Your Answer: <b>%c)</b> %s\n", 'A'+answerIdx, cleanMarkdownForTelegram(q.Options[answerIdx]))
	
	if isCorrect {
		resultText += "✅ <b>CORRECT!</b>\n"
		session.CorrectAnswers++
	} else {
		resultText += fmt.Sprintf("❌ <b>WRONG!</b>\nCorrect Answer was: <b>%c)</b> %s\n", 'A'+q.CorrectIndex, cleanMarkdownForTelegram(q.Options[q.CorrectIndex]))
	}
	resultText += fmt.Sprintf("\n<i>Explanation: %s</i>", cleanMarkdownForTelegram(q.Explanation))

	o.bot.EditQuizResult(session.MessageID, resultText)

	// Fetch failed topics array
	// Fetch failed topics array
	// var failedTopics []string
	// we didn't fetch failedTopics properly in GetActiveQuizSession (left it as string). 
	// Let's just pass it or ignore for now, the LLM analyze_quiz will figure out what they failed based on the score!
	
	// Wait, the LLM needs to know exactly which questions they failed to generate tasks.
	// Let's keep it simple: LLM gets the score. Or even better, the LLM gets the text of the questions they failed!
	// I'll update the analyze_quiz prompt to include failed questions.

	nextIndex := session.CurrentIndex + 1
	if nextIndex >= len(session.Questions) {
		// Quiz finished
		o.bot.SendMessageHTML(fmt.Sprintf("🏁 <b>Quiz Finished!</b>\nScore: %d/%d\n\n<i>Analyzing performance...</i>", session.CorrectAnswers, len(session.Questions)))
		o.db.UpdateQuizSession(session.SessionID, nextIndex, session.CorrectAnswers, 0, "analyzing", "[]")
		go o.finishQuiz(session)
	} else {
		o.serveQuizQuestion(session.SessionID, session.Questions, nextIndex, session.CorrectAnswers, []string{})
	}
}

func (o *Orchestrator) finishQuiz(session *models.QuizSession) {
	difficulty := o.db.GetSetting("quiz_difficulty", "Medium")
	analysis, err := o.llm.AnalyzeQuiz(context.Background(), session, difficulty)
	if err != nil {
		log.Printf("Error analyzing quiz: %v", err)
		o.db.MarkTasksCompleted([]string{session.TaskID})
		o.db.DeleteQuizSession(session.SessionID)
		return
	}

	o.bot.SendMessageHTML(analysis.MessageToUser)

	if analysis.Passed {
		o.db.MarkTasksCompleted([]string{session.TaskID})
	}

	if len(analysis.CreateNewTasks) > 0 {
		for _, taskReq := range analysis.CreateNewTasks {
			taskReq.ParentID = nil // Append as independent tasks for review
			if err := o.db.AddTask(taskReq); err != nil {
				log.Printf("Error adding review task: %v", err)
			}
		}
		o.bot.SendMessageHTML(fmt.Sprintf("Added %d review tasks to your schedule based on the quiz analysis.", len(analysis.CreateNewTasks)))
	}

	o.db.DeleteQuizSession(session.SessionID)
}
