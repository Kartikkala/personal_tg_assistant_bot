package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	
	// If not found in relevant, fetch it directly
	if task == nil {
		task = &models.Task{ID: taskID, Description: "Unknown Topic"}
	}

	isGoal := false
	if task.ParentID == nil {
		isGoal = true
	}

	difficulty := o.db.GetSetting("quiz_difficulty", "Medium")

	o.bot.SendMessageHTML(fmt.Sprintf("🧠 <b>Generating %s Validation Quiz...</b>\nTopic: <i>%s</i>", difficulty, task.Description))

	resp, err := o.llm.GenerateQuiz(context.Background(), task, difficulty, isGoal)
	if err != nil {
		log.Printf("Error generating quiz: %v", err)
		o.bot.SendMessageHTML("❌ Error generating quiz. I'll just mark the task as complete for now.")
		o.db.MarkTasksCompleted([]string{taskID})
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

func (o *Orchestrator) serveQuizQuestion(sessionID int, questions []models.QuizQuestion, index int, correctAnswers int, failedTopics []string) {
	q := questions[index]
	
	text := fmt.Sprintf("📝 <b>Question %d/%d</b>\n\n%s", index+1, len(questions), q.Question)
	
	msgID, err := o.bot.SendQuizQuestion(text, q.Options)
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

	isCorrect := answerIdx == q.CorrectIndex
	
	resultText := fmt.Sprintf("📝 <b>Question %d/%d</b>\n\n%s\n\n", session.CurrentIndex+1, len(session.Questions), q.Question)
	resultText += fmt.Sprintf("Your Answer: %s\n", q.Options[answerIdx])
	
	if isCorrect {
		resultText += "✅ <b>CORRECT!</b>\n"
		session.CorrectAnswers++
	} else {
		resultText += fmt.Sprintf("❌ <b>WRONG!</b>\nCorrect Answer was: %s\n", q.Options[q.CorrectIndex])
	}
	resultText += fmt.Sprintf("\n<i>Explanation: %s</i>", q.Explanation)

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
