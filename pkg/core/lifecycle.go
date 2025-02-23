package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/LambdaTest/synapse/config"
	"github.com/LambdaTest/synapse/pkg/errs"
	"github.com/LambdaTest/synapse/pkg/fileutils"
	"github.com/LambdaTest/synapse/pkg/global"
	"github.com/LambdaTest/synapse/pkg/lumber"
)

const (
	endpointPostTestResults = "http://localhost:9876/results"
)

var endpointPostTestList string
var endpointNeuronReport string

// NewPipeline creates and returns a new Pipeline instance
func NewPipeline(cfg *config.NucleusConfig, logger lumber.Logger) (*Pipeline, error) {
	return &Pipeline{
		Cfg:    cfg,
		Logger: logger,
		HttpClient: http.Client{
			Timeout: 45 * time.Second,
		},
	}, nil
}

//Start starts pipeline lifecycle
func (pl *Pipeline) Start(ctx context.Context) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var errRemark string
	startTime := time.Now()

	pl.Logger.Debugf("Starting pipeline.....")
	pl.Logger.Debugf("Fetching config")

	endpointPostTestList = global.NeuronHost + "/test-list"
	endpointNeuronReport = global.NeuronHost + "/report"
	// fetch configuration
	payload, err := pl.PayloadManager.FetchPayload(ctx, pl.Cfg.PayloadAddress)
	if err != nil {
		pl.Logger.Fatalf("error while fetching payload: %v", err)
	}

	err = pl.PayloadManager.ValidatePayload(ctx, payload)
	if err != nil {
		pl.Logger.Fatalf("error while validating payload %v", err)
	}

	pl.Logger.Debugf("Payload for current task: %+v \n", *payload)

	if pl.Cfg.CoverageMode {
		if err := pl.CoverageService.MergeAndUpload(ctx, payload); err != nil {
			pl.Logger.Fatalf("error while merge and upload coverage files %v", err)
		}
		os.Exit(0)
	}

	oauth, err := pl.SecretParser.GetOauthSecret(global.OauthSecretPath)
	if err != nil {
		pl.Logger.Fatalf("failed to get oauth secret %v", err)
	}

	// set payload on pipeline object
	pl.Payload = payload
	if pl.Cfg.ParseMode {
		err = pl.GitManager.CloneYML(ctx, payload, oauth.Data.AccessToken)
		if err != nil {
			pl.Logger.Fatalf("failed to clone YML for build ID: %s, error: %v", payload.BuildID, err)
		}
		if err = pl.ParserService.PerformParsing(payload); err != nil {
			pl.Logger.Fatalf("error while parsing YML for build ID: %s, error: %v", payload.BuildID, err)
		}
		os.Exit(0)
	}

	taskPayload := &TaskPayload{
		TaskID:      payload.TaskID,
		BuildID:     payload.BuildID,
		RepoSlug:    payload.RepoSlug,
		RepoLink:    payload.RepoLink,
		OrgID:       payload.OrgID,
		RepoID:      payload.RepoID,
		CommitID:    payload.TargetCommit,
		GitProvider: payload.GitProvider,
		StartTime:   startTime,
		Status:      Running,
	}
	if pl.Cfg.DiscoverMode {
		taskPayload.Type = DiscoveryTask
	} else {
		taskPayload.Type = ExecutionTask
	}

	// marking task to running state
	if err := pl.Task.UpdateStatus(taskPayload); err != nil {
		pl.Logger.Fatalf("failed to update task status %v", err)
	}

	// update task status when pipeline exits
	defer func() {
		taskPayload.EndTime = time.Now()
		if p := recover(); p != nil {
			pl.Logger.Errorf("panic stack trace: %v", p)
			taskPayload.Status = Error
			taskPayload.Remark = errs.GenericUserFacingBEErrRemark
		} else if err != nil {
			if err == context.Canceled {
				taskPayload.Status = Aborted
				taskPayload.Remark = "Task aborted"
			} else {
				taskPayload.Status = Error
				taskPayload.Remark = errRemark
			}
		}
		if err := pl.Task.UpdateStatus(taskPayload); err != nil {
			pl.Logger.Fatalf("failed to update task status %v", err)
		}
	}()

	coverageDir := filepath.Join(global.CodeCoveragParentDir, payload.OrgID, payload.RepoID, payload.TargetCommit)
	pl.Logger.Infof("Cloning repo ...")
	err = pl.GitManager.Clone(ctx, pl.Payload, oauth.Data.AccessToken)
	if err != nil {
		pl.Logger.Errorf("Unable to clone repo '%s': %s", payload.RepoLink, err)
		errRemark = fmt.Sprintf("Unable to clone repo: %s", payload.RepoLink)
		return err
	}

	// load tas yaml file
	tasConfig, err := pl.TASConfigManager.LoadConfig(ctx, payload.TasFileName, payload.EventType, false)
	if err != nil {
		pl.Logger.Errorf("Unable to load tas yaml file, error: %v", err)
		errRemark = err.Error()
		return err
	}

	pl.Logger.Infof("Tas yaml: %+v", tasConfig)

	// set testing taskID, orgID and buildID as environment variable
	os.Setenv("TASK_ID", payload.TaskID)
	os.Setenv("ORG_ID", payload.OrgID)
	os.Setenv("BUILD_ID", payload.BuildID)
	//set commit_id as environment variable
	os.Setenv("COMMIT_ID", payload.TargetCommit)
	//set repo_id as environment variable
	os.Setenv("REPO_ID", payload.RepoID)
	//set coverage_dir as environment variable
	os.Setenv("CODE_COVERAGE_DIR", coverageDir)
	os.Setenv("BRANCH_NAME", payload.BranchName)
	os.Setenv("ENV", pl.Cfg.Env)
	os.Setenv("TAS_PARALLELISM", strconv.Itoa(tasConfig.Parallelism))
	os.Setenv("ENDPOINT_POST_TEST_LIST", endpointPostTestList)
	os.Setenv("ENDPOINT_POST_TEST_RESULTS", endpointPostTestResults)
	os.Setenv("REPO_ROOT", global.RepoDir)
	os.Setenv("BLOCKLISTED_TESTS_FILE", global.BlocklistedFileLocation)

	if tasConfig.NodeVersion != nil {
		nodeVersion := tasConfig.NodeVersion.String()
		// Running the `source` command in a directory where .nvmrc is present, exits with exitCode 3
		// https://github.com/nvm-sh/nvm/issues/1985
		// TODO [good-to-have]: Auto-read and install from .nvmrc file, if present
		command := []string{"source", "/home/nucleus/.nvm/nvm.sh",
			"&&", "nvm", "install", nodeVersion}
		pl.Logger.Infof("Using user-defined node version: %v", nodeVersion)
		err = pl.ExecutionManager.ExecuteInternalCommands(ctx, InstallNodeVer, command, "", nil, nil)
		if err != nil {
			pl.Logger.Errorf("Unable to install user-defined nodeversion %v", err)
			errRemark = errs.GenericUserFacingBEErrRemark
			return err
		}
		origPath := os.Getenv("PATH")
		os.Setenv("PATH", fmt.Sprintf("/home/nucleus/.nvm/versions/node/v%s/bin:%s", nodeVersion, origPath))
	}

	if payload.CollectCoverage {
		if err = fileutils.CreateIfNotExists(coverageDir, true); err != nil {
			pl.Logger.Errorf("failed to create coverage directory %v", err)
			errRemark = errs.GenericUserFacingBEErrRemark
			return err
		}
	}

	err = pl.TestBlockListService.GetBlockListedTests(ctx, tasConfig, payload.RepoID)
	if err != nil {
		pl.Logger.Errorf("Unable to fetch blocklisted tests: %v", err)
		errRemark = errs.GenericUserFacingBEErrRemark
		return err
	}

	// read secrets
	secretMap, err := pl.SecretParser.GetRepoSecret(global.RepoSecretPath)
	if err != nil {
		pl.Logger.Errorf("Error in fetching Repo secrets %v", err)
		errRemark = errs.GenericUserFacingBEErrRemark
		return err
	}

	cacheKey := fmt.Sprintf("%s/%s/%s", payload.OrgID, payload.RepoID, tasConfig.Cache.Key)
	// TODO:  download from cdn
	if err = pl.CacheStore.Download(ctx, cacheKey); err != nil {
		pl.Logger.Errorf("Unable to download cache: %v", err)
		errRemark = errs.GenericUserFacingBEErrRemark
		return err
	}

	if tasConfig.Prerun != nil {
		pl.Logger.Infof("Running pre-run steps")
		err = pl.ExecutionManager.ExecuteUserCommands(ctx, PreRun, payload, tasConfig.Prerun, secretMap)
		if err != nil {
			pl.Logger.Errorf("Unable to run pre-run steps %v", err)
			errRemark = "Error occurred in pre-run steps"
			return err
		}
	}
	err = pl.ExecutionManager.ExecuteInternalCommands(ctx, InstallRunners, global.InstallRunnerCmd, global.RepoDir, nil, nil)
	if err != nil {
		pl.Logger.Errorf("Unable to install custom runners %v", err)
		errRemark = errs.GenericUserFacingBEErrRemark
		return err
	}

	if pl.Cfg.DiscoverMode {
		pl.Logger.Infof("Identifying changed files ...")
		diff, err := pl.DiffManager.GetChangedFiles(ctx, payload, oauth.Data.AccessToken)
		if err != nil {
			pl.Logger.Errorf("Unable to identify changed files %s", err)
			errRemark = "Error occurred in fetching diff from GitHub"
			return err
		}

		// discover test cases
		err = pl.TestDiscoveryService.Discover(ctx, tasConfig, pl.Payload, secretMap, diff)
		if err != nil {
			pl.Logger.Errorf("Unable to perform test discovery: %+v", err)
			errRemark = "Error occurred in discovering tests"
			return err
		}
		// mark status as passed
		taskPayload.Status = Passed

	}

	if pl.Cfg.ExecuteMode {
		// execute test cases
		executionResult, err := pl.TestExecutionService.Run(ctx, tasConfig, pl.Payload, coverageDir, secretMap)
		if err != nil {
			pl.Logger.Infof("Unable to perform test execution: %v", err)
			errRemark = "Error occurred in executing tests"
			return err
		}

		if err = pl.sendStats(*executionResult); err != nil {
			pl.Logger.Errorf("error while sending test reports %v", err)
			errRemark = errs.GenericUserFacingBEErrRemark
			return err
		}
		taskPayload.Status = Passed
		for i := 0; i < len(executionResult.TestPayload); i++ {
			testResult := &executionResult.TestPayload[i]
			if testResult.Status == "failed" {
				taskPayload.Status = Failed
				break
			}
		}

		if tasConfig.Postrun != nil {
			pl.Logger.Infof("Running post-run steps")
			err = pl.ExecutionManager.ExecuteUserCommands(ctx, PostRun, payload, tasConfig.Postrun, secretMap)
			if err != nil {
				pl.Logger.Errorf("Unable to run post-run steps %v", err)
				errRemark = "Error occurred in pre-run steps"
				return err
			}
		}
	}
	if err = pl.CacheStore.Upload(ctx, cacheKey, tasConfig.Cache.Paths...); err != nil {
		pl.Logger.Errorf("Unable to upload cache: %v", err)
		errRemark = errs.GenericUserFacingBEErrRemark
		return err
	}
	pl.Logger.Debugf("Cache uploaded successfully")
	pl.Logger.Debugf("Completed pipeline")

	return nil
}

func (pl *Pipeline) sendStats(payload ExecutionResult) error {
	reqBody, err := json.Marshal(payload)
	if err != nil {
		pl.Logger.Errorf("failed to marshal request body %v", err)
		return err
	}

	req, err := http.NewRequest(http.MethodPost, endpointNeuronReport, bytes.NewBuffer(reqBody))
	if err != nil {
		pl.Logger.Errorf("failed to create new request %v", err)
		return err
	}

	resp, err := pl.HttpClient.Do(req)

	if err != nil {
		pl.Logger.Errorf("error while sending reports %v", err)
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		pl.Logger.Errorf("error while sending reports, non 200 status")
		return errors.New("non 200 status")
	}
	return nil
}
