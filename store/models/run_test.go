package models_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/smartcontractkit/chainlink/adapters"
	"github.com/smartcontractkit/chainlink/internal/cltest"
	"github.com/smartcontractkit/chainlink/store/models"
	"github.com/smartcontractkit/chainlink/utils"
	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
	null "gopkg.in/guregu/null.v3"
)

func TestJobRuns_RetrievingFromDBWithError(t *testing.T) {
	t.Parallel()
	store, cleanup := cltest.NewStore()
	defer cleanup()

	job, initr := cltest.NewJobWithWebInitiator()
	jr := job.NewRun(initr)
	jr.Result = cltest.RunResultWithError(fmt.Errorf("bad idea"))
	err := store.Save(&jr)
	assert.NoError(t, err)

	run := &models.JobRun{}
	err = store.One("ID", jr.ID, run)
	assert.NoError(t, err)
	assert.True(t, run.Result.HasError())
	assert.Equal(t, "bad idea", run.Result.Error())
}

func TestJobRun_UnfinishedTaskRuns(t *testing.T) {
	t.Parallel()

	store, cleanup := cltest.NewStore()
	defer cleanup()
	jobRunner, cleanup := cltest.NewJobRunner(store)
	defer cleanup()
	jobRunner.Start()

	job, initiator := cltest.NewJobWithWebInitiator()
	job.Tasks = []models.TaskSpec{
		{Type: adapters.TaskTypeNoOp},
		{Type: adapters.TaskTypeNoOpPend},
		{Type: adapters.TaskTypeNoOp},
	}
	assert.NoError(t, store.SaveJob(&job))
	run := job.NewRun(initiator)
	assert.NoError(t, store.Save(&run))
	assert.Equal(t, run.TaskRuns, run.UnfinishedTaskRuns())

	store.RunChannel.Send(run.ID, models.RunResult{}, nil)
	cltest.WaitForJobRunStatus(t, store, run, models.RunStatusPendingConfirmations)

	store.One("ID", run.ID, &run)
	assert.Equal(t, run.TaskRuns[1:], run.UnfinishedTaskRuns())
}

func TestTaskRun_Runnable(t *testing.T) {
	t.Parallel()

	job, initr := cltest.NewJobWithLogInitiator()
	tests := []struct {
		name                 string
		creationHeight       *hexutil.Big
		currentHeight        *models.IndexableBlockNumber
		minimumConfirmations uint64
		want                 bool
	}{
		{"creation nil current nil minconfs 0", nil, nil, 0, true},
		{"creation 1 current nil minconfs 0", cltest.NewBigHexInt(1), nil, 0, true},
		{"creation 1 current 1 minconfs 0", cltest.NewBigHexInt(1), cltest.IndexableBlockNumber(1), 0, true},
		{"creation 1 current 1 minconfs 1", cltest.NewBigHexInt(1), cltest.IndexableBlockNumber(1), 1, true},
		{"creation 1 current 2 minconfs 1", cltest.NewBigHexInt(1), cltest.IndexableBlockNumber(2), 1, true},
		{"creation 1 current 2 minconfs 2", cltest.NewBigHexInt(1), cltest.IndexableBlockNumber(2), 2, true},
		{"creation 1 current 2 minconfs 3", cltest.NewBigHexInt(1), cltest.IndexableBlockNumber(2), 3, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			jr := job.NewRun(initr)
			if test.creationHeight != nil {
				jr.CreationHeight = test.creationHeight
			}

			assert.Equal(t, test.want, jr.Runnable(test.currentHeight, test.minimumConfirmations))
		})
	}
}

func TestTaskRun_Merge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"preserves task field",
			`{"url":"https://NEW.example.com/api"}`,
			`{"url":"https://OLD.example.com/api"}`},
		{"add field",
			`{"extra":1}`,
			`{"url":"https://OLD.example.com/api","extra":1}`},
		{"preserves task field and adds new field",
			`{"url":"https://NEW.example.com/api","extra":1}`,
			`{"url":"https://OLD.example.com/api","extra":1}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			orig := `{"url":"https://OLD.example.com/api"}`
			tr := models.TaskRun{
				Task: models.TaskSpec{
					Params: models.JSON{Result: gjson.Parse(orig)},
					Type:   adapters.TaskTypeHTTPGet,
				},
			}
			input := cltest.JSONFromString(test.input)

			merged, err := tr.MergeTaskParams(input)
			assert.NoError(t, err)
			assert.JSONEq(t, test.want, merged.Task.Params.String())
			assert.JSONEq(t, orig, tr.Task.Params.String())
		})
	}
}

func TestRunResult_Value(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		json        string
		want        string
		wantErrored bool
	}{
		{"string", `{"value": "100", "other": "101"}`, "100", false},
		{"integer", `{"value": 100}`, "", true},
		{"float", `{"value": 100.01}`, "", true},
		{"boolean", `{"value": true}`, "", true},
		{"null", `{"value": null}`, "", true},
		{"no key", `{"other": 100}`, "", true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var data models.JSON
			assert.NoError(t, json.Unmarshal([]byte(test.json), &data))
			rr := models.RunResult{Data: data}

			val, err := rr.Value()
			assert.Equal(t, test.want, val)
			assert.Equal(t, test.wantErrored, (err != nil))
		})
	}
}

func TestRunResult_WithError(t *testing.T) {
	t.Parallel()

	rr := models.RunResult{}

	assert.Equal(t, models.RunStatusUnstarted, rr.Status)

	rr = rr.WithError(errors.New("this blew up"))

	assert.Equal(t, models.RunStatusErrored, rr.Status)
	assert.Equal(t, cltest.NullString("this blew up"), rr.ErrorMessage)
}

func TestRunResult_Merge(t *testing.T) {
	t.Parallel()

	inProgress := models.RunStatusInProgress
	pending := models.RunStatusPendingBridge
	errored := models.RunStatusErrored

	nullString := cltest.NullString(nil)
	jrID := utils.NewBytes32ID()
	tests := []struct {
		name             string
		originalData     string
		originalError    null.String
		originalStatus   models.RunStatus
		originalJRID     string
		inData           string
		inError          null.String
		inStatus         models.RunStatus
		inJRID           string
		wantData         string
		wantErrorMessage null.String
		wantStatus       models.RunStatus
		wantJRID         string
		wantErrored      bool
	}{
		{"merging data",
			`{"value":"old&busted","unique":"1"}`, nullString, inProgress, jrID,
			`{"value":"newHotness","and":"!"}`, nullString, inProgress, jrID,
			`{"value":"newHotness","unique":"1","and":"!"}`, nullString, inProgress, jrID, false},
		{"original error throws",
			`{"value":"old"}`, cltest.NullString("old problem"), errored, jrID,
			`{}`, nullString, inProgress, jrID,
			`{"value":"old"}`, cltest.NullString("old problem"), errored, jrID, true},
		{"error override",
			`{"value":"old"}`, nullString, inProgress, jrID,
			`{}`, cltest.NullString("new problem"), errored, jrID,
			`{"value":"old"}`, cltest.NullString("new problem"), errored, jrID, false},
		{"original job run ID",
			`{"value":"old"}`, nullString, inProgress, jrID,
			`{}`, nullString, inProgress, "",
			`{"value":"old"}`, nullString, inProgress, jrID, false},
		{"job run ID override",
			`{"value":"old"}`, nullString, inProgress, utils.NewBytes32ID(),
			`{}`, nullString, inProgress, jrID,
			`{"value":"old"}`, nullString, inProgress, jrID, false},
		{"original pending",
			`{"value":"old"}`, nullString, pending, jrID,
			`{}`, nullString, inProgress, jrID,
			`{"value":"old"}`, nullString, pending, jrID, false},
		{"pending override",
			`{"value":"old"}`, nullString, inProgress, jrID,
			`{}`, nullString, pending, jrID,
			`{"value":"old"}`, nullString, pending, jrID, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := models.RunResult{
				Data:         models.JSON{Result: gjson.Parse(test.originalData)},
				ErrorMessage: test.originalError,
				JobRunID:     test.originalJRID,
				Status:       test.originalStatus,
			}
			in := models.RunResult{
				Data:         cltest.JSONFromString(test.inData),
				ErrorMessage: test.inError,
				JobRunID:     test.inJRID,
				Status:       test.inStatus,
			}
			merged, err := original.Merge(in)
			assert.Equal(t, test.wantErrored, err != nil)

			assert.JSONEq(t, test.originalData, original.Data.String())
			assert.Equal(t, test.originalError, original.ErrorMessage)
			assert.Equal(t, test.originalJRID, original.JobRunID)
			assert.Equal(t, test.originalStatus, original.Status)

			assert.JSONEq(t, test.inData, in.Data.String())
			assert.Equal(t, test.inError, in.ErrorMessage)
			assert.Equal(t, test.inJRID, in.JobRunID)
			assert.Equal(t, test.inStatus, in.Status)

			assert.JSONEq(t, test.wantData, merged.Data.String())
			assert.Equal(t, test.wantErrorMessage, merged.ErrorMessage)
			assert.Equal(t, test.wantJRID, merged.JobRunID)
			assert.Equal(t, test.wantStatus, merged.Status)
		})
	}
}
