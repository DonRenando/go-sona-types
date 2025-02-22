//
// Copyright 2018-present Sonatype Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

// Package iq has definitions and functions for processing golang purls with Nexus IQ Server
package iq

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sonatype-nexus-community/go-sona-types/cyclonedx"
	"github.com/sonatype-nexus-community/go-sona-types/ossindex"
	"github.com/sonatype-nexus-community/go-sona-types/ossindex/types"
	"github.com/sonatype-nexus-community/go-sona-types/useragent"
)

const internalApplicationIDURL = "/api/v2/applications?publicId="

const thirdPartyAPILeft = "/api/v2/scan/applications/"

const thirdPartyAPIRight = "/sources/nancy?stageId="

// StatusURLResult is a struct to let the consumer know what the response from Nexus IQ Server was
type StatusURLResult struct {
	PolicyAction          string `json:"policyAction"`
	ReportHTMLURL         string `json:"reportHtmlUrl"`
	AbsoluteReportHTMLURL string `json:"-"`
	IsError               bool   `json:"isError"`
	ErrorMessage          string `json:"errorMessage"`
}

// Valid policy action values
const (
	PolicyActionNone    = "None"
	PolicyActionWarning = "Warning"
	PolicyActionFailure = "Failure"
)

// Internal types for use by this package, don't need to expose them
type applicationResponse struct {
	Applications []application `json:"applications"`
}

type application struct {
	ID string `json:"id"`
}

type thirdPartyAPIResult struct {
	StatusURL string `json:"statusUrl"`
}

var statusURLResp StatusURLResult

type resultError struct {
	finished bool
	err      error
}

// ServerError is a custom error type that can be used to differentiate between
// regular errors and errors specific to handling IQ Server
type ServerError struct {
	Err     error
	Message string
}

func (i *ServerError) Error() string {
	if i.Err != nil {
		return fmt.Sprintf("An error occurred: %s, err: %s", i.Message, i.Err.Error())
	}
	return fmt.Sprintf("An error occurred: %s", i.Message)
}

type ServerErrorMissingLicense struct {
}

func (i *ServerErrorMissingLicense) Error() string {
	return "error accessing nexus iq server: No valid product license installed"
}

// IServer is an interface that can be used for mocking the Server struct
type IServer interface {
	AuditWithSbom(s string) (StatusURLResult, error)
	AuditPackages(p []string) (StatusURLResult, error)
}

// Server is a struct that holds the IQ Server options, logger and other properties related to
// communicating with Nexus IQ Server
type Server struct {
	// Options is the accepted Options for communicating with IQ Server, and OSS Index (see Options struct)
	// for more information
	Options Options
	// logLady is the internal name of the logger, and accepts a pointer to a *logrus.Logger
	logLady *logrus.Logger
	// agent is a pointer to a *useragent.Agent struct, used for setting the User-Agent when communicating
	// with IQ Server and OSS Index
	agent *useragent.Agent
	// tries is an internal variable for keeping track of how many times IQ Server has been polled
	tries int
}

// Options is a struct for setting options on the Server struct
type Options struct {
	// User is the IQ Server user you intend to authenticate with
	User string
	// Token is the IQ Server token you intend to authenticate with
	Token string
	// Stage is the IQ Server stage you intend to generate a report with (ex: develop, build, release, etc...)
	Stage string
	// Application is the IQ Server public application ID you intend to run the audit with
	Application string
	// Server is the IQ Server base URL (ex: http://localhost:8070)
	Server string
	// MaxRetries is the maximum amount of times to long poll IQ Server for results
	MaxRetries int
	// Tool is the client-id you want to have set in your User-Agent string (ex: nancy-client)
	Tool string
	// Version is the version of the tool you are writing, that you want set in your User-Agent string (ex: 1.0.0)
	Version string
	// User is the OSS Index user you intend to authenticate with
	OSSIndexUser string
	// Token is the OSS Index token you intend to authenticate with
	OSSIndexToken string
	// DBCacheName is the name of the OSS Index cache you'd like to use (ex: nancy-cache)
	DBCacheName string
	// TTL is the maximum time you want items to live in the DB Cache before being evicted (defaults to 12 hours)
	TTL time.Time
	// PollInterval is the time you want to wait between polls of IQ Server (defaults to 1 second)
	PollInterval time.Duration
}

// New is intended to be the way to obtain a iq instance, where you control the options
func New(logger *logrus.Logger, options Options) (server *Server, err error) {
	if logger == nil {
		err = fmt.Errorf("missing logger")
		return
	}

	if err = validateRequiredOption(options, "Application"); err != nil {
		return
	}
	if err = validateRequiredOption(options, "Server"); err != nil {
		return
	}
	if err = validateRequiredOption(options, "User"); err != nil {
		return
	}
	if err = validateRequiredOption(options, "Token"); err != nil {
		return
	}

	if options.PollInterval == 0 {
		logger.Trace("Setting Poll Interval to 1 second since it wasn't set explicitly")
		options.PollInterval = 1 * time.Second
	}

	if options.TTL.IsZero() {
		logger.Trace("Setting TTL to 12 hours since it wasn't set explicitly")
		options.TTL = time.Now().Local().Add(time.Hour * 12)
	}

	if strings.HasSuffix(options.Server, "/") {
		options.Server = strings.TrimSuffix(options.Server, "/")
	}

	ua := useragent.New(logger, useragent.Options{ClientTool: options.Tool, Version: options.Version})

	server = &Server{logLady: logger, Options: options, tries: 0, agent: ua}
	return
}

func validateRequiredOption(options Options, optionName string) (err error) {
	e := reflect.ValueOf(&options).Elem()
	zero := e.FieldByName(optionName).IsZero()
	if zero {
		err = fmt.Errorf("missing options.%s", optionName)
	}
	return
}

// AuditWithSbom accepts an sbom string, and will submit this to
// Nexus IQ Server for audit, and return a struct of StatusURLResult
func (i *Server) AuditWithSbom(sbom string) (StatusURLResult, error) {
	i.logLady.WithFields(logrus.Fields{
		"sbom":           sbom,
		"application_id": i.Options.Application,
	}).Info("Beginning audit with IQ using provided SBOM")

	if i.Options.User == "admin" && i.Options.Token == "admin123" {
		i.logLady.Info("Warning user of questionable life choices related to username and password")
		warnUserOfBadLifeChoices()
	}

	internalID, err := i.getInternalApplicationID(i.Options.Application)
	if internalID == "" && err != nil {
		i.logLady.Error("Internal ID not obtained from Nexus IQ")
		return statusURLResp, err
	}

	return i.audit(sbom, internalID)
}

// AuditPackages accepts a slice of purls, and configuration, and will submit these to
// Nexus IQ Server for audit, and return a struct of StatusURLResult
func (i *Server) AuditPackages(purls []string) (StatusURLResult, error) {
	i.logLady.WithFields(logrus.Fields{
		"purls":          purls,
		"application_id": i.Options.Application,
	}).Info("Beginning audit with IQ")

	if i.Options.User == "admin" && i.Options.Token == "admin123" {
		i.logLady.Info("Warning user of questionable life choices related to username and password")
		warnUserOfBadLifeChoices()
	}

	internalID, err := i.getInternalApplicationID(i.Options.Application)
	if internalID == "" && err != nil {
		i.logLady.Error("Internal ID not obtained from Nexus IQ")
		return statusURLResp, err
	}

	ossIndexOptions := types.Options{
		Username:    i.Options.OSSIndexUser,
		Token:       i.Options.OSSIndexToken,
		DBCacheName: i.Options.DBCacheName,
		TTL:         i.Options.TTL,
	}

	ossi := ossindex.New(i.logLady, ossIndexOptions)

	resultsFromOssIndex, err := ossi.AuditPackages(purls)
	if err != nil {
		return statusURLResp, &ServerError{
			Err:     err,
			Message: "There was an issue auditing packages using OSS Index",
		}
	}

	dx := cyclonedx.Default(i.logLady)

	sbom := dx.FromCoordinates(resultsFromOssIndex)
	i.logLady.WithField("sbom", sbom).Debug("Obtained cyclonedx SBOM")

	return i.audit(sbom, internalID)
}

func (i *Server) audit(sbom string, internalID string) (StatusURLResult, error) {
	i.logLady.WithFields(logrus.Fields{
		"internal_id": internalID,
		"sbom":        sbom,
	}).Debug("Submitting to Third Party API")
	statusURL, err := i.submitToThirdPartyAPI(sbom, internalID)
	if err != nil {
		return statusURLResp, &ServerError{
			Err:     err,
			Message: "There was an issue submitting to the Third Party API",
		}
	}
	if statusURL == "" {
		i.logLady.Error("StatusURL not obtained from Third Party API")
		return statusURLResp, &ServerError{
			Err:     fmt.Errorf("There was an issue submitting your sbom to the Nexus IQ Third Party API, sbom: %s", sbom),
			Message: "There was an issue obtaining a StatusURL",
		}
	}

	statusURLResp = StatusURLResult{}

	finishedChan := make(chan resultError)

	go func() {
		defer close(finishedChan)
		for {
			select {
			case <-finishedChan:
				return
			default:
				if errPoll := i.pollIQServer(fmt.Sprintf("%s/%s", i.Options.Server, statusURL), finishedChan); errPoll != nil {
					finishedChan <- resultError{finished: true, err: errPoll}
					return
				}
				i.logLady.Trace("waiting to poll Nexus IQ")
				time.Sleep(i.Options.PollInterval)
			}
		}
	}()

	r := <-finishedChan
	return statusURLResp, r.err
}

func (i *Server) getInternalApplicationID(applicationID string) (string, error) {
	client := &http.Client{}

	req, err := http.NewRequest(
		"GET",
		fmt.Sprintf("%s%s%s", i.Options.Server, internalApplicationIDURL, applicationID),
		nil,
	)
	if err != nil {
		return "", &ServerError{
			Err:     err,
			Message: "Request to get internal application id failed",
		}
	}

	req.SetBasicAuth(i.Options.User, i.Options.Token)
	req.Header.Set("User-Agent", i.agent.GetUserAgent())

	resp, err := client.Do(req)
	if err != nil {
		return "", &ServerError{
			Err:     err,
			Message: "There was an error communicating with Nexus IQ Server to get your internal application ID",
		}
	}

	if resp.StatusCode == http.StatusPaymentRequired {
		i.logLady.WithField("resp_status_code", resp.Status).Error("Error accessing Nexus IQ Server due to product license")
		return "", &ServerErrorMissingLicense{}
	}

	//noinspection GoUnhandledErrorResult
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return "", &ServerError{
				Err:     err,
				Message: "There was an error retrieving the bytes of the response for getting your internal application ID from Nexus IQ Server",
			}
		}

		var response applicationResponse
		err = json.Unmarshal(bodyBytes, &response)
		if err != nil {
			return "", &ServerError{
				Err:     err,
				Message: "failed to unmarshal response",
			}
		}

		if response.Applications != nil && len(response.Applications) > 0 {
			i.logLady.WithFields(logrus.Fields{
				"internal_id": response.Applications[0].ID,
			}).Debug("Retrieved internal ID from Nexus IQ Server")

			return response.Applications[0].ID, nil
		}

		i.logLady.WithFields(logrus.Fields{
			"application_id": applicationID,
		}).Error("Unable to retrieve an internal ID for the specified public application ID")

		return "", &ServerError{
			Err:     fmt.Errorf("Unable to retrieve an internal ID for the specified public application ID: %s", applicationID),
			Message: "Unable to retrieve an internal ID",
		}
	}

	// read body of response with error
	//noinspection GoUnhandledErrorResult
	defer resp.Body.Close()
	var b []byte
	b, err = io.ReadAll(resp.Body)
	if err != nil {
		i.logLady.Error(err)
	}
	respBody := string(b)

	i.logLady.WithFields(logrus.Fields{
		"status_code": resp.StatusCode,
		"status":      resp.Status,
		"respBody":    respBody,
	}).Error("Error communicating with Nexus IQ Server application endpoint")
	return "", &ServerError{
		Err: fmt.Errorf("unable to communicate with Nexus IQ Server, status code: %d, status: %s, body: %s",
			resp.StatusCode, resp.Status, respBody),
		Message: "Unable to communicate with Nexus IQ Server",
	}
}

func (i *Server) submitToThirdPartyAPI(sbom string, internalID string) (string, error) {
	i.logLady.Debug("Beginning to submit to Third Party API")
	client := &http.Client{}

	url := fmt.Sprintf("%s%s", i.Options.Server, fmt.Sprintf("%s%s%s%s", thirdPartyAPILeft, internalID, thirdPartyAPIRight, i.Options.Stage))
	i.logLady.WithField("url", url).Debug("Crafted URL for submission to Third Party API")

	req, err := http.NewRequest(
		"POST",
		url,
		bytes.NewBuffer([]byte(sbom)),
	)
	if err != nil {
		return "", &ServerError{
			Err:     err,
			Message: "Could not POST to Nexus iQ Third Party API",
		}
	}

	req.SetBasicAuth(i.Options.User, i.Options.Token)
	req.Header.Set("User-Agent", i.agent.GetUserAgent())
	req.Header.Set("Content-Type", "application/xml")

	resp, err := client.Do(req)
	if err != nil {
		return "", &ServerError{
			Err:     err,
			Message: "There was an issue communicating with the Nexus IQ Third Party API",
		}
	}

	//noinspection GoUnhandledErrorResult
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		i.logLady.WithField("body", string(bodyBytes)).Info("Request accepted")
		if err != nil {
			return "", &ServerError{
				Err:     err,
				Message: "There was an issue submitting your sbom to the Nexus IQ Third Party API",
			}
		}

		var response thirdPartyAPIResult
		err = json.Unmarshal(bodyBytes, &response)
		if err != nil {
			return "", &ServerError{
				Err:     err,
				Message: "Could not unmarshal response from IQ server",
			}
		}
		return response.StatusURL, err
	}

	// something went wrong
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		i.logLady.Error(err)
		// do not return to allow the ServerError below to be returned
	}
	i.logLady.WithFields(logrus.Fields{
		"body":        string(bodyBytes),
		"status_code": resp.StatusCode,
		"status":      resp.Status,
	}).Info("Request not accepted")
	return "", &ServerError{
		Err:     fmt.Errorf("status_code: %d, body: %s, err: %+v", resp.StatusCode, string(bodyBytes), err),
		Message: "There was an issue submitting your sbom to the Nexus IQ Third Party API",
	}
}

func (i *Server) pollIQServer(statusURL string, finished chan resultError) error {
	i.logLady.WithFields(logrus.Fields{
		"attempt_number": i.tries,
		"max_retries":    i.Options.MaxRetries,
		"status_url":     statusURL,
	}).Trace("Polling Nexus IQ for response")
	if i.tries > i.Options.MaxRetries {
		i.logLady.WithField("retries", i.Options.MaxRetries).Error("Maximum tries exceeded, finished polling, consider bumping up Max Retries")
		err := fmt.Errorf("exceeded max retries: %d", i.Options.MaxRetries)
		finished <- resultError{finished: true, err: err}
		return &ServerError{Err: err, Message: "exceeded max retries"}
	}

	client := &http.Client{}
	req, err := http.NewRequest("GET", statusURL, nil)
	if err != nil {
		return &ServerError{
			Err:     err,
			Message: "Could not poll IQ server",
		}
	}

	req.SetBasicAuth(i.Options.User, i.Options.Token)

	req.Header.Set("User-Agent", i.agent.GetUserAgent())

	resp, err := client.Do(req)

	if err != nil {
		finished <- resultError{finished: true, err: err}
		return &ServerError{
			Err:     err,
			Message: "There was an error polling Nexus IQ Server",
		}
	}

	//noinspection GoUnhandledErrorResult
	defer resp.Body.Close()

	i.logLady.WithFields(logrus.Fields{
		"resp.StatusCode": resp.StatusCode,
	}).Trace("Nexus IQ polling status")

	if resp.StatusCode == http.StatusOK {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return &ServerError{
				Err:     err,
				Message: "There was an error with processing the response from polling Nexus IQ Server",
			}
		}

		var response StatusURLResult
		err = json.Unmarshal(bodyBytes, &response)
		if err != nil {
			return &ServerError{
				Err:     err,
				Message: "Could not unmarshal response from IQ server",
			}
		}

		i.logLady.WithFields(logrus.Fields{
			"response": response,
		}).Trace("Nexus IQ polling response")

		statusURLResp = response
		if response.IsError {
			finished <- resultError{finished: true, err: nil}
		}

		statusURLResp.populateAbsoluteURL(i.Options.Server)
		finished <- resultError{finished: true, err: nil}
	}
	i.tries++
	fmt.Print(".")
	return err
}

func (i *StatusURLResult) populateAbsoluteURL(iqServerBaseURL string) {
	parsedReportURL, _ := url.Parse(statusURLResp.ReportHTMLURL)
	if parsedReportURL.IsAbs() {
		statusURLResp.AbsoluteReportHTMLURL = parsedReportURL.String()
		return
	}
	statusURLResp.AbsoluteReportHTMLURL =
		strings.TrimRight(iqServerBaseURL, "/") +
			"/" +
			strings.TrimLeft(parsedReportURL.Path, "/")
}

func warnUserOfBadLifeChoices() {
	fmt.Println()
	fmt.Println("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
	fmt.Println("!!!! WARNING : You are using the default username and password for Nexus IQ. !!!!")
	fmt.Println("!!!! You are strongly encouraged to change these, and use a token.           !!!!")
	fmt.Println("!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!")
	fmt.Println()
}
