package web_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/smartcontractkit/chainlink/core/internal/cltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPipelineJobSpecErrorsController_Delete(t *testing.T) {
	app, client, _, jID, _, _ := setupJobSpecsControllerTestsWithJobs(t)

	description := "job spec error description"

	app.JobORM().RecordError(context.Background(), jID, description)

	// FindJob -> find error
	j, err := app.JobORM().FindJob(context.Background(), jID)
	require.NoError(t, err)
	require.Len(t, j.JobSpecErrors, 1)
	jse := j.JobSpecErrors[0]

	resp, cleanup := client.Delete(fmt.Sprintf("/v2/pipeline/job_spec_errors/%v", jse.ID))
	defer cleanup()
	cltest.AssertServerResponse(t, resp, http.StatusNoContent)

	// FindJob -> errors are empty
	j, err = app.JobORM().FindJob(context.Background(), j.ID)
	require.NoError(t, err)
	require.Len(t, j.JobSpecErrors, 0)
}

func TestPipelineJobSpecErrorsController_Delete_NotFound(t *testing.T) {
	_, client, _, _, _, _ := setupJobSpecsControllerTestsWithJobs(t)

	resp, cleanup := client.Delete("/v2/pipeline/job_spec_errors/1")
	defer cleanup()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "Response should be not found")
}
