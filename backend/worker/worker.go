package worker

import (
	"container/list"
	"context"
	"encoding/json"
	"io"
	"math"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/pkg/errors"
	e "github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

	parse "github.com/highlight-run/highlight/backend/event-parse"
	"github.com/highlight-run/highlight/backend/hlog"
	"github.com/highlight-run/highlight/backend/model"
	storage "github.com/highlight-run/highlight/backend/object-storage"
	"github.com/highlight-run/highlight/backend/payload"
	mgraph "github.com/highlight-run/highlight/backend/private-graph/graph"
	"github.com/highlight-run/highlight/backend/util"
	"github.com/highlight-run/workerpool"
)

// Worker is a job runner that parses sessions
const MIN_INACTIVE_DURATION = 10

type Worker struct {
	Resolver *mgraph.Resolver
	S3Client *storage.StorageClient
}

func (w *Worker) pushToObjectStorageAndWipe(ctx context.Context, s *model.Session, migrationState *string, eventsFile *os.File, resourcesFile *os.File, messagesFile *os.File) error {
	if err := w.Resolver.DB.Model(&model.Session{}).Where(
		&model.Session{Model: model.Model{ID: s.ID}},
	).Updates(
		&model.Session{MigrationState: migrationState},
	).Error; err != nil {
		return errors.Wrap(err, "error updating session to processed status")
	}
	sessionPayloadSize, err := w.S3Client.PushFileToS3(ctx, s.ID, s.ProjectID, eventsFile, storage.S3SessionsPayloadBucketName, storage.SessionContents)
	// If this is unsucessful, return early (we treat this session as if it is stored in psql).
	if err != nil {
		return errors.Wrap(err, "error pushing session payload to s3")
	}

	resourcePayloadSize, err := w.S3Client.PushFileToS3(ctx, s.ID, s.ProjectID, resourcesFile, storage.S3SessionsPayloadBucketName, storage.NetworkResources)
	if err != nil {
		return errors.Wrap(err, "error pushing network payload to s3")
	}

	messagePayloadSize, err := w.S3Client.PushFileToS3(ctx, s.ID, s.ProjectID, messagesFile, storage.S3SessionsPayloadBucketName, storage.ConsoleMessages)
	if err != nil {
		return errors.Wrap(err, "error pushing network payload to s3")
	}

	var totalPayloadSize int64
	if sessionPayloadSize != nil {
		totalPayloadSize += *sessionPayloadSize
	}
	if resourcePayloadSize != nil {
		totalPayloadSize += *resourcePayloadSize
	}
	if messagePayloadSize != nil {
		totalPayloadSize += *messagePayloadSize
	}

	// Mark this session as stored in S3.
	if err := w.Resolver.DB.Model(&model.Session{}).Where(
		&model.Session{Model: model.Model{ID: s.ID}},
	).Updates(
		&model.Session{ObjectStorageEnabled: &model.T, PayloadSize: &totalPayloadSize},
	).Error; err != nil {
		return errors.Wrap(err, "error updating session to storage enabled")
	}

	// Delete all the events_objects in the DB.
	if err := w.Resolver.DB.Unscoped().Where(&model.EventsObject{SessionID: s.ID}).Delete(&model.EventsObject{}).Error; err != nil {
		return errors.Wrapf(err, "error deleting all event records")
	}
	if err := w.Resolver.DB.Unscoped().Where(&model.ResourcesObject{SessionID: s.ID}).Delete(&model.ResourcesObject{}).Error; err != nil {
		return errors.Wrap(err, "error deleting all network resource records")
	}
	if err := w.Resolver.DB.Unscoped().Where(&model.MessagesObject{SessionID: s.ID}).Delete(&model.MessagesObject{}).Error; err != nil {
		return errors.Wrap(err, "error deleting all messages")
	}
	return nil
}

func (w *Worker) scanSessionPayload(ctx context.Context, s *model.Session, eventsFile *os.File, resourcesFile *os.File, messagesFile *os.File) (*payload.PayloadManager, error) {
	manager := payload.NewPayloadManager(eventsFile, resourcesFile, messagesFile)

	// Fetch/write events.
	eventRows, err := w.Resolver.DB.Model(&model.EventsObject{}).Where(&model.EventsObject{SessionID: s.ID}).Order("created_at asc").Rows()
	if err != nil {
		return nil, errors.Wrap(err, "error retrieving events objects")
	}
	var numberOfRows int64 = 0
	eventsWriter := manager.Events.Writer()
	for eventRows.Next() {
		eventObject := model.EventsObject{}
		err := w.Resolver.DB.ScanRows(eventRows, &eventObject)
		if err != nil {
			return nil, errors.Wrap(err, "error scanning event row")
		}
		if err := eventsWriter.Write(&eventObject); err != nil {
			return nil, errors.Wrap(err, "error writing event row")
		}
		numberOfRows += 1
	}
	manager.Events.Length = numberOfRows

	// Fetch/write resources.
	resourcesRows, err := w.Resolver.DB.Model(&model.ResourcesObject{}).Where(&model.ResourcesObject{SessionID: s.ID}).Order("created_at asc").Rows()
	if err != nil {
		return nil, errors.Wrap(err, "error retrieving resources objects")
	}
	resourceWriter := manager.Resources.Writer()
	numberOfRows = 0
	for resourcesRows.Next() {
		resourcesObject := model.ResourcesObject{}
		err := w.Resolver.DB.ScanRows(resourcesRows, &resourcesObject)
		if err != nil {
			return nil, errors.Wrap(err, "error scanning resource row")
		}
		if err := resourceWriter.Write(&resourcesObject); err != nil {
			return nil, errors.Wrap(err, "error writing resource row")
		}
		numberOfRows += 1
	}
	manager.Resources.Length = numberOfRows

	// Fetch/write messages.
	messageRows, err := w.Resolver.DB.Model(&model.MessagesObject{}).Where(&model.MessagesObject{SessionID: s.ID}).Order("created_at asc").Rows()
	if err != nil {
		return nil, errors.Wrap(err, "error retrieving messages objects")
	}

	numberOfRows = 0
	messagesWriter := manager.Messages.Writer()
	for messageRows.Next() {
		messageObject := model.MessagesObject{}
		if err := w.Resolver.DB.ScanRows(messageRows, &messageObject); err != nil {
			return nil, errors.Wrap(err, "error scanning message row")
		}
		if err := messagesWriter.Write(&messageObject); err != nil {
			return nil, errors.Wrap(err, "error writing messages object")
		}
		numberOfRows += 1
	}
	manager.Messages.Length = numberOfRows

	// Measure payload sizes.
	eventInfo, err := eventsFile.Stat()
	if err != nil {
		return nil, errors.Wrap(err, "error getting event file info")
	}
	hlog.Histogram("worker.processSession.eventPayloadSize", float64(eventInfo.Size()), nil, 1) //nolint

	resourceInfo, err := resourcesFile.Stat()
	if err != nil {
		return nil, errors.Wrap(err, "error getting resource file info")
	}
	hlog.Histogram("worker.processSession.resourcePayloadSize", float64(resourceInfo.Size()), nil, 1) //nolint

	messagesInfo, err := messagesFile.Stat()
	if err != nil {
		return nil, errors.Wrap(err, "error getting message file info")
	}
	hlog.Histogram("worker.processSession.messagePayloadSize", float64(messagesInfo.Size()), nil, 1) //nolint

	return manager, nil
}

func CreateFile(name string) (func(), *os.File, error) {
	file, err := os.Create(name)
	if err != nil {
		return nil, nil, errors.Wrap(err, "error creating file")
	}
	return func() {
		err := file.Close()
		if err != nil {
			log.Error(e.Wrap(err, "failed to close file"))
			return
		}
		err = os.Remove(file.Name())
		if err != nil {
			log.Error(e.Wrap(err, "failed to remove file"))
			return
		}
	}, file, nil
}

func (w *Worker) processSession(ctx context.Context, s *model.Session) error {

	sessionIdString := os.Getenv("SESSION_FILE_PATH_PREFIX") + strconv.FormatInt(int64(s.ID), 10)

	// Create files.
	eventsClose, eventsFile, err := CreateFile(sessionIdString + ".events.txt")
	if err != nil {
		return errors.Wrap(err, "error creating events file")
	}
	defer eventsClose()
	resourcesClose, resourcesFile, err := CreateFile(sessionIdString + ".resources.txt")
	if err != nil {
		return errors.Wrap(err, "error creating events file")
	}
	defer resourcesClose()
	messagesClose, messagesFile, err := CreateFile(sessionIdString + ".messages.txt")
	if err != nil {
		return errors.Wrap(err, "error creating events file")
	}
	defer messagesClose()

	payloadManager, err := w.scanSessionPayload(ctx, s, eventsFile, resourcesFile, messagesFile)
	if err != nil {
		return errors.Wrap(err, "error scanning session payload")
	}

	//Delete the session if there's no events.
	if payloadManager.Events.Length == 0 {
		log.WithFields(log.Fields{"session_id": s.ID, "project_id": s.ProjectID}).Warnf("there are no events for session: %d", s.ID)
		if err := w.Resolver.DB.Select(clause.Associations).Delete(&model.Session{Model: model.Model{ID: s.ID}}).Error; err != nil {
			return errors.Wrap(err, "error trying to delete associations for session with no events")
		}
		if err := w.Resolver.DB.Delete(&model.Session{Model: model.Model{ID: s.ID}}).Error; err != nil {
			return errors.Wrap(err, "error trying to delete session with no events")
		}
		return nil
	}

	// need to reset file pointer to beginning of file for reading
	for _, file := range []*os.File{eventsFile, resourcesFile, messagesFile} {
		_, err = file.Seek(0, io.SeekStart)
		if err != nil {
			log.WithField("file_name", file.Name()).Errorf("error seeking to beginning of file: %v", err)
		}
	}
	activeDuration := time.Duration(0)
	var (
		firstEventTimestamp time.Time
		lastEventTimestamp  time.Time
	)
	p := payload.NewPayloadReadWriter(eventsFile)
	re := p.Reader()
	hasNext := true
	clickEventQueue := list.New()
	var rageClickSets []*model.RageClickEvent
	var currentlyInRageClickSet bool
	for hasNext {
		se, err := re.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return e.Wrap(err, "error reading next line")
			}
			hasNext = false
		}
		if se != nil && *se != "" {
			eventsObject := model.EventsObject{Events: *se}
			o := processEventChunk(&processEventChunkInput{
				EventsChunk:             &eventsObject,
				ClickEventQueue:         clickEventQueue,
				FirstEventTimestamp:     firstEventTimestamp,
				LastEventTimestamp:      lastEventTimestamp,
				RageClickSets:           rageClickSets,
				CurrentlyInRageClickSet: currentlyInRageClickSet,
			})
			if o.Error != nil {
				return e.Wrap(err, "error processing event chunk")
			}
			firstEventTimestamp = o.FirstEventTimestamp
			lastEventTimestamp = o.LastEventTimestamp
			activeDuration += o.CalculatedDuration
			rageClickSets = o.RageClickSets
			currentlyInRageClickSet = o.CurrentlyInRageClickSet
		}
	}
	for i, r := range rageClickSets {
		r.SessionSecureID = s.SecureID
		r.ProjectID = s.ProjectID
		rageClickSets[i] = r
	}
	if len(rageClickSets) > 0 {
		if err := w.Resolver.DB.Create(&rageClickSets).Error; err != nil {
			log.Error(e.Wrap(err, "error creating rage click sets"))
		}
	}

	// Calculate total session length and write the length to the session.
	sessionTotalLength := CalculateSessionLength(firstEventTimestamp, lastEventTimestamp)
	sessionTotalLengthInMilliseconds := sessionTotalLength.Milliseconds()

	// Delete the session if the length of the session is 0.
	// 1. Nothing happened in the session
	// 2. A web crawler visited the page and produced no events
	if activeDuration == 0 {
		log.WithFields(log.Fields{"session_id": s.ID, "project_id": s.ProjectID}).Warn("active duration is 0 for session, deleting...")
		if err := w.Resolver.DB.Where(&model.EventsObject{SessionID: s.ID}).Delete(&model.EventsObject{}).Error; err != nil {
			return errors.Wrap(err, "error trying to delete events_object for session of length 0ms")
		}
		if err := w.Resolver.DB.Select(clause.Associations).Delete(&model.Session{Model: model.Model{ID: s.ID}}).Error; err != nil {
			return errors.Wrap(err, "error trying to delete associations for session with length 0")
		}
		if err := w.Resolver.DB.Delete(&model.Session{Model: model.Model{ID: s.ID}}).Error; err != nil {
			return errors.Wrap(err, "error trying to delete session of length 0ms")
		}
		return nil
	}

	if err := w.Resolver.DB.Model(&model.Session{}).Where(
		&model.Session{Model: model.Model{ID: s.ID}},
	).Updates(
		model.Session{
			Processed:    &model.T,
			Length:       sessionTotalLengthInMilliseconds,
			ActiveLength: activeDuration.Milliseconds(),
		},
	).Error; err != nil {
		return errors.Wrap(err, "error updating session to processed status")
	}

	// Update session count on dailydb
	currentDate := time.Date(s.CreatedAt.UTC().Year(), s.CreatedAt.UTC().Month(), s.CreatedAt.UTC().Day(), 0, 0, 0, 0, time.UTC)
	dailySession := &model.DailySessionCount{}
	if err := w.Resolver.DB.
		Where(&model.DailySessionCount{
			ProjectID: s.ProjectID,
			Date:      &currentDate,
		}).Attrs(&model.DailySessionCount{Count: 0}).
		FirstOrCreate(&dailySession).Error; err != nil {
		return e.Wrap(err, "Error creating new daily session")
	}

	if err := w.Resolver.DB.
		Where(&model.DailySessionCount{Model: model.Model{ID: dailySession.ID}}).
		Updates(&model.DailySessionCount{Count: dailySession.Count + 1}).Error; err != nil {
		return e.Wrap(err, "Error incrementing session count in db")
	}

	var g errgroup.Group
	projectID := s.ProjectID
	project := &model.Project{}
	if err := w.Resolver.DB.Where(&model.Project{Model: model.Model{ID: s.ProjectID}}).First(&project).Error; err != nil {
		return e.Wrap(err, "error querying project")
	}

	g.Go(func() error {
		// Sending New User Alert
		// if is not new user, return
		if s.FirstTime == nil || !*s.FirstTime {
			return nil
		}
		var sessionAlert model.SessionAlert
		if err := w.Resolver.DB.Model(&model.SessionAlert{}).Where(&model.SessionAlert{Alert: model.Alert{ProjectID: projectID}}).Where("type IS NULL OR type=?", model.AlertType.NEW_USER).First(&sessionAlert).Error; err != nil {
			return e.Wrapf(err, "[project_id: %d] error fetching new user alert", projectID)
		}

		// check if session was produced from an excluded environment
		excludedEnvironments, err := sessionAlert.GetExcludedEnvironments()
		if err != nil {
			return e.Wrapf(err, "[project_id: %d] error getting excluded environments from new user alert", projectID)
		}
		isExcludedEnvironment := false
		for _, env := range excludedEnvironments {
			if env != nil && *env == s.Environment {
				isExcludedEnvironment = true
				break
			}
		}
		if isExcludedEnvironment {
			return nil
		}

		// get produced user properties from session
		userProperties, err := s.GetUserProperties()
		if err != nil {
			return e.Wrapf(err, "[project_id: %d] error getting user properties from new user alert", s.ProjectID)
		}

		workspace, err := w.Resolver.GetWorkspace(project.WorkspaceID)
		if err != nil {
			return e.Wrapf(err, "[project_id: %d] error querying workspace", s.ProjectID)
		}

		// send Slack message
		err = sessionAlert.SendSlackAlert(&model.SendSlackAlertInput{Workspace: workspace, SessionSecureID: s.SecureID, UserIdentifier: s.Identifier, UserProperties: userProperties})
		if err != nil {
			return e.Wrapf(err, "[project_id: %d] error sending slack message for new user alert", projectID)
		}
		return nil
	})

	g.Go(func() error {
		// Sending Track Properties Alert
		var sessionAlert model.SessionAlert
		if err := w.Resolver.DB.Model(&model.SessionAlert{}).Where(&model.SessionAlert{Alert: model.Alert{ProjectID: projectID}}).Where("type=?", model.AlertType.TRACK_PROPERTIES).First(&sessionAlert).Error; err != nil {
			return e.Wrapf(err, "[project_id: %d] error fetching track properties alert", projectID)
		}

		excludedEnvironments, err := sessionAlert.GetExcludedEnvironments()
		if err != nil {
			return e.Wrapf(err, "[project_id: %d] error getting excluded environments from track properties alert", projectID)
		}
		isExcludedEnvironment := false
		for _, env := range excludedEnvironments {
			if env != nil && *env == s.Environment {
				isExcludedEnvironment = true
				break
			}
		}
		if isExcludedEnvironment {
			return nil
		}

		// get matched track properties between the alert and session
		trackProperties, err := sessionAlert.GetTrackProperties()
		if err != nil {
			return e.Wrap(err, "error getting track properties from session")
		}
		var trackPropertyIds []int
		for _, trackProperty := range trackProperties {
			trackPropertyIds = append(trackPropertyIds, trackProperty.ID)
		}
		stmt := w.Resolver.DB.Model(&model.Field{}).
			Where(&model.Field{ProjectID: projectID, Type: "track"}).
			Where("id IN (SELECT field_id FROM session_fields WHERE session_id=?)", s.ID).
			Where("id IN ?", trackPropertyIds)
		var matchedFields []*model.Field
		if err := stmt.Find(&matchedFields).Error; err != nil {
			return e.Wrap(err, "error querying matched fields by session_id")
		}
		if len(matchedFields) < 1 {
			return nil
		}

		workspace, err := w.Resolver.GetWorkspace(project.WorkspaceID)
		if err != nil {
			return e.Wrap(err, "error querying workspace")
		}

		// send Slack message
		err = sessionAlert.SendSlackAlert(&model.SendSlackAlertInput{Workspace: workspace, SessionSecureID: s.SecureID, UserIdentifier: s.Identifier, MatchedFields: matchedFields})
		if err != nil {
			return e.Wrap(err, "error sending track properties alert slack message")
		}
		return nil
	})

	g.Go(func() error {
		// Sending User Properties Alert
		var sessionAlert model.SessionAlert
		if err := w.Resolver.DB.Model(&model.SessionAlert{}).Where(&model.SessionAlert{Alert: model.Alert{ProjectID: projectID}}).Where("type=?", model.AlertType.USER_PROPERTIES).First(&sessionAlert).Error; err != nil {
			return e.Wrapf(err, "[project_id: %d] error fetching user properties alert", projectID)
		}

		// check if session was produced from an excluded environment
		excludedEnvironments, err := sessionAlert.GetExcludedEnvironments()
		if err != nil {
			return e.Wrapf(err, "[project_id: %d] error getting excluded environments from user properties alert", projectID)
		}
		isExcludedEnvironment := false
		for _, env := range excludedEnvironments {
			if env != nil && *env == s.Environment {
				isExcludedEnvironment = true
				break
			}
		}
		if isExcludedEnvironment {
			return nil
		}

		// get matched user properties between the alert and session
		userProperties, err := sessionAlert.GetUserProperties()
		if err != nil {
			return e.Wrap(err, "error getting user properties from session")
		}
		var userPropertyIds []int
		for _, userProperty := range userProperties {
			userPropertyIds = append(userPropertyIds, userProperty.ID)
		}
		stmt := w.Resolver.DB.Model(&model.Field{}).
			Where(&model.Field{ProjectID: projectID, Type: "user"}).
			Where("id IN (SELECT field_id FROM session_fields WHERE session_id=?)", s.ID).
			Where("id IN ?", userPropertyIds)
		var matchedFields []*model.Field
		if err := stmt.Find(&matchedFields).Error; err != nil {
			return e.Wrap(err, "error querying matched fields by session_id")
		}
		if len(matchedFields) < 1 {
			return nil
		}

		workspace, err := w.Resolver.GetWorkspace(project.WorkspaceID)
		if err != nil {
			return e.Wrap(err, "error querying workspace")
		}

		// send Slack message
		err = sessionAlert.SendSlackAlert(&model.SendSlackAlertInput{Workspace: workspace, SessionSecureID: s.SecureID, UserIdentifier: s.Identifier, MatchedFields: matchedFields})
		if err != nil {
			return e.Wrapf(err, "error sending user properties alert slack message")
		}
		return nil
	})

	g.Go(func() error {
		// Sending session init alert
		var sessionAlert model.SessionAlert
		if err := w.Resolver.DB.Model(&model.SessionAlert{}).Where(&model.SessionAlert{Alert: model.Alert{ProjectID: projectID}}).
			Where("type=?", model.AlertType.NEW_SESSION).First(&sessionAlert).Error; err != nil {
			return e.Wrapf(err, "[project_id: %d] error fetching new session alert", projectID)
		}

		// check if session was produced from an excluded environment
		excludedEnvironments, err := sessionAlert.GetExcludedEnvironments()
		if err != nil {
			return e.Wrapf(err, "[project_id: %d] error getting excluded environments from new session alert", projectID)
		}
		isExcludedEnvironment := false
		for _, env := range excludedEnvironments {
			if env != nil && *env == s.Environment {
				isExcludedEnvironment = true
				break
			}
		}
		if isExcludedEnvironment {
			return nil
		}

		workspace, err := w.Resolver.GetWorkspace(project.WorkspaceID)
		if err != nil {
			return e.Wrap(err, "error querying workspace")
		}

		// send Slack message
		err = sessionAlert.SendSlackAlert(&model.SendSlackAlertInput{Workspace: workspace, SessionSecureID: s.SecureID, UserIdentifier: s.Identifier})
		if err != nil {
			return e.Wrapf(err, "[project_id: %d] error sending slack message for new session alert", projectID)
		}
		return nil
	})

	// Waits for all goroutines to finish, then returns the first non-nil error (if any).
	if err := g.Wait(); err != nil {
		log.WithFields(log.Fields{"session_id": s.ID, "project_id": s.ProjectID}).Error(e.Wrap(err, "error sending slack alert"))
	}

	// Upload to s3 and wipe from the db.
	if os.Getenv("ENABLE_OBJECT_STORAGE") == "true" {
		state := "normal"
		if err := w.pushToObjectStorageAndWipe(ctx, s, &state, eventsFile, resourcesFile, messagesFile); err != nil {
			log.WithFields(log.Fields{"session_id": s.ID, "project_id": s.ProjectID}).Error(e.Wrap(err, "error pushing to object and wiping from db"))
		}
	}
	return nil
}

// Start begins the worker's tasks.
func (w *Worker) Start() {
	ctx := context.Background()
	payloadLookbackPeriod := 60 // a session must be stale for at least this long to be processed
	if util.IsDevEnv() {
		payloadLookbackPeriod = 8
	}
	go reportProcessSessionCount(w.Resolver.DB, payloadLookbackPeriod)
	for {
		time.Sleep(1 * time.Second)
		now := time.Now()
		processSessionLimit := 10000
		someSecondsAgo := now.Add(time.Duration(-1*payloadLookbackPeriod) * time.Second)
		sessions := []*model.Session{}
		sessionsSpan, ctx := tracer.StartSpanFromContext(ctx, "worker.sessionsQuery", tracer.ResourceName("worker.sessionsQuery"))
		if err := w.Resolver.DB.
			Where("(payload_updated_at < ? OR payload_updated_at IS NULL) AND (processed = ?)", someSecondsAgo, false).
			Limit(processSessionLimit).Find(&sessions).Error; err != nil {
			log.Errorf("error querying unparsed, outdated sessions: %v", err)
			sessionsSpan.Finish()
			continue
		}
		rand.Seed(time.Now().UnixNano())
		rand.Shuffle(len(sessions), func(i, j int) {
			sessions[i], sessions[j] = sessions[j], sessions[i]
		})
		// Sends a "count" metric to datadog so that we can see how many sessions are being queried.
		hlog.Histogram("worker.sessionsQuery.sessionCount", float64(len(sessions)), nil, 1) //nolint
		sessionsSpan.Finish()
		type SessionLog struct {
			SessionID int
			ProjectID int
		}
		sessionIds := []SessionLog{}
		for _, session := range sessions {
			sessionIds = append(sessionIds, SessionLog{SessionID: session.ID, ProjectID: session.ProjectID})
		}
		if len(sessionIds) > 0 {
			log.Infof("sessions that will be processed: %v", sessionIds)
		}

		wp := workerpool.New(40)
		wp.SetPanicHandler(func() {
			if rec := recover(); rec != nil {
				buf := make([]byte, 64<<10)
				buf = buf[:runtime.Stack(buf, false)]
				log.Errorf("panic: %+v\n%s", rec, buf)
			}
		})
		// process 80 sessions at a time.
		for _, session := range sessions {
			session := session
			ctx := ctx
			wp.SubmitRecover(func() {
				span, ctx := tracer.StartSpanFromContext(ctx, "worker.operation", tracer.ResourceName("worker.processSession"))
				if err := w.processSession(ctx, session); err != nil {
					log.WithField("session_id", session.ID).Error(e.Wrap(err, "error processing main session"))
					span.Finish(tracer.WithError(e.Wrapf(err, "error processing session: %v", session.ID)))
					return
				}
				hlog.Incr("sessionsProcessed", nil, 1)
				span.Finish()
			})
		}
	}
}

// CalculateSessionLength gets the session length given two sets of ReplayEvents.
func CalculateSessionLength(first time.Time, last time.Time) (d time.Duration) {
	if first.IsZero() {
		return d
	}
	d = last.Sub(first)
	return d
}

type processEventChunkInput struct {
	// EventsChunk represents the chunk of events to be processed in this iteration of processEventChunk
	EventsChunk *model.EventsObject
	// ClickEventQueue is a queue containing the last 2 seconds worth of clustered click events
	ClickEventQueue *list.List
	// CurrentlyInRageClickSet denotes whether the currently parsed event is within a rage click set
	CurrentlyInRageClickSet bool
	// CurrentlyInRageClickSet denotes whether the currently parsed event is within a rage click set
	RageClickSets []*model.RageClickEvent
	// FirstEventTimestamp represents the timestamp for the first event
	FirstEventTimestamp time.Time
	// LastEventTimestamp represents the timestamp for the first event
	LastEventTimestamp time.Time
}

type processEventChunkOutput struct {
	// DidEventsChunkChange denotes whether the events chunk was altered and needs to be updated in the stored file
	DidEventsChunkChange bool
	// CurrentlyInRageClickSet denotes whether the currently parsed event is within a rage click set
	CurrentlyInRageClickSet bool
	// RageClickSets contains all rage click sets that will be inserted into the db
	RageClickSets []*model.RageClickEvent
	// FirstEventTimestamp represents the timestamp for the first event
	FirstEventTimestamp time.Time
	// LastEventTimestamp represents the timestamp for the first event
	LastEventTimestamp time.Time
	// CalculatedDuration represents the calculated active duration for the current event chunk
	CalculatedDuration time.Duration
	// Error
	Error error
}

func processEventChunk(input *processEventChunkInput) (o processEventChunkOutput) {
	var events *parse.ReplayEvents
	var err error
	if input == nil {
		o.Error = errors.New("processEventChunkInput cannot be nil")
		return o
	}
	if input.EventsChunk == nil {
		o.Error = errors.New("EventsChunk cannot be nil")
		return o
	}
	if input.ClickEventQueue == nil {
		o.Error = errors.New("ClickEventQueue cannot be nil")
		return o
	}
	events, err = parse.EventsFromString(input.EventsChunk.Events)
	if err != nil {
		o.Error = err
		return o
	}
	o.FirstEventTimestamp = input.FirstEventTimestamp
	o.LastEventTimestamp = input.LastEventTimestamp
	o.CurrentlyInRageClickSet = input.CurrentlyInRageClickSet
	o.RageClickSets = input.RageClickSets
	for _, event := range events.Events {
		if event == nil {
			continue
		}
		if event.Type == parse.IncrementalSnapshot {
			var diff time.Duration
			if !o.LastEventTimestamp.IsZero() {
				diff = event.Timestamp.Sub(o.LastEventTimestamp)
				if diff.Seconds() <= MIN_INACTIVE_DURATION {
					o.CalculatedDuration += diff
				}
			}
			o.LastEventTimestamp = event.Timestamp
			if o.FirstEventTimestamp.IsZero() {
				o.FirstEventTimestamp = event.Timestamp
			}

			// purge old clicks
			var toRemove []*list.Element
			for element := input.ClickEventQueue.Front(); element != nil; element = element.Next() {
				if event.Timestamp.Sub(element.Value.(*parse.ReplayEvent).Timestamp) > time.Second*5 {
					toRemove = append(toRemove, element)
				}
			}

			for _, elem := range toRemove {
				input.ClickEventQueue.Remove(elem)
			}

			var mouseInteractionEventData parse.MouseInteractionEventData
			err = json.Unmarshal(event.Data, &mouseInteractionEventData)
			if err != nil {
				o.Error = err
				return o
			}
			if mouseInteractionEventData.X == nil || mouseInteractionEventData.Y == nil ||
				mouseInteractionEventData.Type == nil || mouseInteractionEventData.Source == nil {
				// all values must not be nil on a click/touch event
				continue
			}
			if *mouseInteractionEventData.Source != parse.MouseInteraction {
				// Source must be MouseInteraction for a click/touch event
				continue
			}
			if _, ok := map[parse.MouseInteractions]bool{parse.Click: true,
				parse.DblClick: true}[*mouseInteractionEventData.Type]; !ok {
				// Type must be a Click, Double Click, or Touch Start for a click/touch event
				continue
			}

			// save all new click events
			input.ClickEventQueue.PushBack(event)

			numTotal := 0
			rageClick := model.RageClickEvent{
				TotalClicks: 5,
			}
			for element := input.ClickEventQueue.Front(); element != nil; element = element.Next() {
				el := element.Value.(*parse.ReplayEvent)
				if el == event {
					continue
				}
				var prev *parse.MouseInteractionEventData
				err = json.Unmarshal(el.Data, &prev)
				if err != nil {
					o.Error = err
					return o
				}
				first := math.Pow(*mouseInteractionEventData.X-*prev.X, 2)
				second := math.Pow(*mouseInteractionEventData.Y-*prev.Y, 2)
				if math.Sqrt(first+second) <= 32 {
					numTotal += 1
					if !o.CurrentlyInRageClickSet && rageClick.StartTimestamp.IsZero() {
						rageClick.StartTimestamp = el.Timestamp
					}
				}
			}
			if numTotal >= 5 {
				if o.CurrentlyInRageClickSet {
					o.RageClickSets[len(o.RageClickSets)-1].TotalClicks += 1
					o.RageClickSets[len(o.RageClickSets)-1].EndTimestamp = event.Timestamp
				} else {
					o.CurrentlyInRageClickSet = true
					rageClick.EndTimestamp = event.Timestamp
					rageClick.TotalClicks = numTotal
					o.RageClickSets = append(o.RageClickSets, &rageClick)
				}
			} else if o.CurrentlyInRageClickSet {
				o.CurrentlyInRageClickSet = false
			}
		}
	}

	return o
}

func reportProcessSessionCount(db *gorm.DB, lookbackPeriod int) {
	for {
		time.Sleep(5 * time.Second)
		var count int64
		if err := db.Raw(`
			SELECT COUNT(*)
			FROM sessions
			WHERE (
				payload_updated_at < (NOW() - ? * INTERVAL '1 SECOND')
				OR payload_updated_at IS NULL
			)
			AND processed=false;
		`, lookbackPeriod).Scan(&count).Error; err != nil {
			log.Error("error getting count of sessions to process")
			continue
		}
		hlog.Histogram("processSessionsCount", float64(count), nil, 1)
	}
}
