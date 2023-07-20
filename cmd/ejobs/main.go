// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command ejobs supports jobs on ecosystem-metrics.
package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"golang.org/x/pkgsite-metrics/internal/jobs"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

const (
	projectID           = "go-ecosystem"
	uploaderMetadataKey = "uploader"
)

// Common flags
var (
	env    = flag.String("env", "prod", "worker environment (dev or prod)")
	dryRun = flag.Bool("n", false, "print actions but do not execute them")
)

var (
	startFlagSet = flag.NewFlagSet("start", flag.ContinueOnError)
	minImporters = startFlagSet.Int("min", -1, "run on modules with at least this many importers (<0: use server default of 10)")
)

var commands = []command{
	{"list", "",
		"list jobs",
		doList, nil},
	{"show", "JOBID...",
		"display information about jobs in the last 7 days",
		doShow, nil},
	{"cancel", "JOBID...",
		"cancel the jobs",
		doCancel, nil},
	{"start", "-min [MIN_IMPORTERS] BINARY ARGS...",
		"start a job",
		doStart, startFlagSet},
	{"wait", "JOBID",
		"do not exit until JOBID is done",
		doWait, nil},
}

type command struct {
	name   string
	argdoc string
	desc   string
	run    func(context.Context, []string) error
	flags  *flag.FlagSet
}

func main() {
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintln(out, "usage:")
		for _, cmd := range commands {
			fmt.Fprintf(out, "  ejobs %s %s\n", cmd.name, cmd.argdoc)
			fmt.Fprintf(out, "\t%s\n", cmd.desc)
		}
		fmt.Fprintln(out, "\ncommon flags:")
		flag.PrintDefaults()
	}

	flag.Parse()
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		flag.Usage()
		os.Exit(2)
	}
}

var workerURL string

func run(ctx context.Context) error {
	wu := os.Getenv("GO_ECOSYSTEM_WORKER_URL_SUFFIX")
	if wu == "" {
		return errors.New("need GO_ECOSYSTEM_WORKER_URL_SUFFIX environment variable")
	}
	workerURL = fmt.Sprintf("https://%s-%s", *env, wu)
	name := flag.Arg(0)
	for _, cmd := range commands {
		if cmd.name == name {
			return cmd.run(ctx, flag.Args()[1:])
		}
	}
	return fmt.Errorf("unknown command %q", name)
}

func doShow(ctx context.Context, args []string) error {
	ts, err := identityTokenSource(ctx)
	if err != nil {
		return err
	}
	for _, jobID := range args {
		if err := showJob(ctx, jobID, ts); err != nil {
			return err
		}
	}
	return nil
}

func showJob(ctx context.Context, jobID string, ts oauth2.TokenSource) error {
	job, err := requestJSON[jobs.Job](ctx, "jobs/describe?jobid="+jobID, ts)
	if err != nil {
		return err
	}
	if *dryRun {
		return nil
	}
	rj := reflect.ValueOf(job).Elem()
	rt := rj.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.IsExported() {
			v := rj.FieldByIndex(f.Index)
			name, _ := strings.CutPrefix(f.Name, "Num")
			fmt.Printf("%s: %v\n", name, v.Interface())
		}
	}
	return nil
}

func doList(ctx context.Context, _ []string) error {
	ts, err := identityTokenSource(ctx)
	if err != nil {
		return err
	}
	joblist, err := requestJSON[[]jobs.Job](ctx, "jobs/list", ts)
	if err != nil {
		return err
	}
	if *dryRun {
		return nil
	}
	d7 := -time.Hour * 24 * 7
	weekBefore := time.Now().Add(d7)
	tw := tabwriter.NewWriter(os.Stdout, 2, 8, 1, ' ', 0)
	fmt.Fprintf(tw, "ID\tUser\tStart Time\tStarted\tFinished\tTotal\tCanceled\n")
	for _, j := range *joblist {
		if j.StartedAt.After(weekBefore) {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%t\n",
				j.ID(), j.User, j.StartedAt.Format(time.RFC3339),
				j.NumStarted,
				j.NumSkipped+j.NumFailed+j.NumErrored+j.NumSucceeded,
				j.NumEnqueued,
				j.Canceled)
		}
	}
	return tw.Flush()
}

func doCancel(ctx context.Context, args []string) error {
	ts, err := identityTokenSource(ctx)
	if err != nil {
		return err
	}
	for _, jobID := range args {
		url := workerURL + "/jobs/cancel?jobid=" + jobID
		if *dryRun {
			fmt.Printf("dryrun: GET %s\n", url)
			continue
		}
		if _, err := httpGet(ctx, url, ts); err != nil {
			return fmt.Errorf("canceling %q: %w", jobID, err)
		}
	}
	return nil
}

func doWait(ctx context.Context, args []string) error {
	ts, err := identityTokenSource(ctx)
	if err != nil {
		return err
	}
	jobID := args[0]
	for {
		job, err := requestJSON[jobs.Job](ctx, "jobs/describe?jobid="+jobID, ts)
		if err != nil {
			return err
		}
		done := job.NumSkipped + job.NumFailed + job.NumErrored + job.NumSucceeded
		if done >= job.NumEnqueued {
			break
		}
		time.Sleep(time.Second)
	}
	fmt.Printf("Job %s finished.\n", jobID)
	return nil
}

func doStart(ctx context.Context, args []string) error {
	// Validate arguments.
	if err := startFlagSet.Parse(args); err != nil {
		return err
	}
	if startFlagSet.NArg() == 0 {
		return errors.New("wrong number of args: want [-min N] BINARY [ARG1 ARG2 ...]")
	}
	binaryFile := startFlagSet.Arg(0)
	if fi, err := os.Stat(binaryFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s does not exist", binaryFile)
		}
		return err
	} else if fi.IsDir() {
		return fmt.Errorf("%s is a directory, not a file", binaryFile)
	} else if err := checkIsLinuxAmd64(binaryFile); err != nil {
		return err
	}
	// Check args to binary for whitespace, which we don't support.

	binaryArgs := startFlagSet.Args()[1:]
	for _, arg := range binaryArgs {
		if strings.IndexFunc(arg, unicode.IsSpace) >= 0 {
			return fmt.Errorf("arg %q contains whitespace: not supported", arg)
		}
	}
	// Copy binary to GCS if it's not already there.
	if canceled, err := uploadAnalysisBinary(ctx, binaryFile); err != nil {
		return err
	} else if canceled {
		return nil
	}
	// Ask the server to enqueue scan tasks.
	its, err := identityTokenSource(ctx)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/analysis/enqueue?binary=%s&user=%s", workerURL, filepath.Base(binaryFile), os.Getenv("USER"))
	if len(binaryArgs) > 0 {
		u += fmt.Sprintf("&args=%s", url.QueryEscape(strings.Join(binaryArgs, " ")))
	}
	if *minImporters >= 0 {
		u += fmt.Sprintf("&min=%d", *minImporters)
	}
	if *dryRun {
		fmt.Printf("dryrun: GET %s\n", u)
		return nil
	}
	body, err := httpGet(ctx, u, its)
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", body)
	return nil
}

// checkIsLinuxAmd64 checks if binaryFile is a linux/amd64 Go
// binary. If not, returns an error with appropriate message.
// Otherwise, returns nil.
func checkIsLinuxAmd64(binaryFile string) error {
	bin, err := os.Open(binaryFile)
	if err != nil {
		return err
	}
	defer bin.Close()

	bi, err := buildinfo.Read(bin)
	if err != nil {
		return err
	}

	var goos, goarch string
	for _, setting := range bi.Settings {
		if setting.Key == "GOOS" {
			goos = setting.Value
		} else if setting.Key == "GOARCH" {
			goarch = setting.Value
		}
	}

	if goos != "linux" || goarch != "amd64" {
		return fmt.Errorf("binary not built for linux/amd64: GOOS=%s GOARCH=%s", goos, goarch)
	}
	return nil
}

// uploadAnalysisBinary copies binaryFile to the GCS location used for
// analysis binaries. The user can cancel the upload if the file with
// the same name is already on GCS, upon which true is returned. Otherwise,
// false is returned.
//
// As an optimization, it skips the upload if the file on GCS has the
// same checksum as the local file.
func uploadAnalysisBinary(ctx context.Context, binaryFile string) (canceled bool, err error) {
	if *dryRun {
		fmt.Printf("dryrun: upload analysis binary %s\n", binaryFile)
		return false, nil
	}
	const bucketName = projectID
	binaryName := filepath.Base(binaryFile)
	objectName := path.Join("analysis-binaries", binaryName)

	ts, err := accessTokenSource(ctx)
	if err != nil {
		return false, err
	}
	c, err := storage.NewClient(ctx, option.WithTokenSource(ts))
	if err != nil {
		return false, err
	}
	defer c.Close()
	bucket := c.Bucket(bucketName)
	object := bucket.Object(objectName)
	attrs, err := object.Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		fmt.Printf("%s binary does not exist on GCS: uploading\n", binaryName)
	} else if err != nil {
		return false, err
	} else if g, w := len(attrs.MD5), md5.Size; g != w {
		return false, fmt.Errorf("len(attrs.MD5) = %d, wanted %d", g, w)

	} else {
		localMD5, err := fileMD5(binaryFile)
		if err != nil {
			return false, err
		}
		if bytes.Equal(localMD5, attrs.MD5) {
			fmt.Printf("Binary %q on GCS has the same checksum: not uploading.\n", binaryName)
			return false, nil
		}
		// Ask the users if they want to overwrite the existing binary
		// while providing more info to help them with their decision.
		updated := attrs.Updated.In(time.Local).Format(time.RFC1123) // use local time zone
		fmt.Printf("The binary %q already exists on GCS.\n", binaryName)
		fmt.Printf("It was last uploaded on %s", updated)
		// Communicate uploader info if available.
		if uploader := attrs.Metadata[uploaderMetadataKey]; uploader != "" {
			fmt.Printf(" by %s", uploader)
		}
		fmt.Println(".")
		fmt.Print("Do you wish to overwrite it? [y/n] ")
		var response string
		fmt.Scanln(&response)
		if r := strings.TrimSpace(response); r != "y" && r != "Y" {
			// Accept "Y" and "y" as confirmation.
			fmt.Println("Cancelling.")
			return true, nil
		}
	}
	fmt.Printf("Uploading.\n")
	if err := copyToGCS(ctx, object, binaryFile); err != nil {
		return false, err
	}

	// Add the uploader information for better messaging in the future.
	toUpdate := storage.ObjectAttrsToUpdate{
		Metadata: map[string]string{uploaderMetadataKey: os.Getenv("USER")},
	}
	// Refetch the object, otherwise attribute uploading won't have effect.
	object = bucket.Object(objectName)
	object.Update(ctx, toUpdate) // disregard errors
	return false, nil
}

// fileMD5 computes the MD5 checksum of the given file.
func fileMD5(filename string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	hash := md5.New()
	if _, err := io.Copy(hash, f); err != nil {
		return nil, err
	}
	return hash.Sum(nil)[:], nil
}

// copyToLocalFile copies the filename to the GCS object.
func copyToGCS(ctx context.Context, object *storage.ObjectHandle, filename string) error {
	src, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer src.Close()
	dest := object.NewWriter(ctx)
	if _, err := io.Copy(dest, src); err != nil {
		return err
	}
	return dest.Close()
}

// requestJSON requests the path from the worker, then reads the returned body
// and unmarshals it as JSON.
func requestJSON[T any](ctx context.Context, path string, ts oauth2.TokenSource) (*T, error) {
	url := workerURL + "/" + path
	if *dryRun {
		fmt.Printf("GET %s\n", url)
		return nil, nil
	}
	body, err := httpGet(ctx, url, ts)
	if err != nil {
		return nil, err
	}
	var t T
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// httpGet makes a GET request to the given URL with the given identity token.
// It reads the body and returns the HTTP response and the body.
func httpGet(ctx context.Context, url string, ts oauth2.TokenSource) (body []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	token, err := ts.Token()
	if err != nil {
		return nil, err
	}
	token.SetAuthHeader(req)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err = io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("reading body (%s): %v", res.Status, err)
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", res.Status, body)
	}
	return body, nil
}

var serviceAccountEmail = fmt.Sprintf("impersonate@%s.iam.gserviceaccount.com", projectID)

func accessTokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
		TargetPrincipal: serviceAccountEmail,
		Scopes:          []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
}

func identityTokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return impersonate.IDTokenSource(ctx, impersonate.IDTokenConfig{
		TargetPrincipal: serviceAccountEmail,
		Audience:        workerURL,
		IncludeEmail:    true,
	})
}
