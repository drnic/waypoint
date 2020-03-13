package lambda

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/hashicorp/go-hclog"
	"github.com/mattn/go-isatty"
	"github.com/mitchellh/devflow/builtin/lambda/runner"
	"github.com/mitchellh/devflow/internal/pkg/status"
	"github.com/mitchellh/devflow/sdk/component"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh/terminal"
)

const (
	DefaultMemory  = 256
	DefaultTimeout = 60
)

type DeployConfig struct {
	Bucket string `hcl:"bucket"`
}

type Deployer struct {
	// L hclog.Logger

	config DeployConfig

	// ID         string
	// Name string
	// Runtime    string
	// ScratchDir string
	// Bucket     string

	roleName string
	roleArn  string
}

func (d *Deployer) Config() (interface{}, error) {
	return &d.config, nil
}

func (d *Deployer) DeployFunc() interface{} {
	return d.Deploy
}

func (d *Deployer) ExecFunc() interface{} {
	return d.Exec
}

func NewDeployer() *Deployer {
	return &Deployer{}
}

const rolePolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Service": "lambda.amazonaws.com"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}`

var ignorePrefixes = []string{
	"etc/", "input/", "root/", "run/", "tmp/", "usr/include/", "usr/share/doc/",
	"usr/share/locale/", "usr/share/man/", "var/cache/", "var/lib/rpm/", "var/lib/yum/",
	"var/log/", "var/task/vendor/bundle/ruby/2.5.0/cache/", "var/task/.bundle", "var/task/.devflow",
	"prebuild",
}

type prefix struct {
	prefix, shift string
}

var layerPrefixes = []string{
	"vendor/bundle/ruby",
}

var shiftPrefix = []prefix{
	{"var/task/vendor/bundle/ruby", "_layer/ruby/gems"},
	{"var/task/", ""},
	{"usr/lib/", "lib/"},
	{"usr/lib64/", "lib/"},
	{"usr/bin/", "bin/"},
}

var sess = session.New(aws.NewConfig().WithRegion("us-west-2"))

func (d *Deployer) SetupRole(L hclog.Logger, app *component.Source) error {
	svc := iam.New(sess)

	d.roleName = "lambda-" + app.App

	L.Info("attempting to retrieve existing role", "role-name", d.roleName)

	queryInput := &iam.GetRoleInput{
		RoleName: aws.String(d.roleName),
	}

	getOut, err := svc.GetRole(queryInput)
	if err == nil {
		d.roleArn = *getOut.Role.Arn
		L.Info("found existing role", "arn", d.roleArn)
		return nil
	}

	L.Info("creating new role")

	input := &iam.CreateRoleInput{
		AssumeRolePolicyDocument: aws.String(rolePolicy),
		Path:                     aws.String("/"),
		RoleName:                 aws.String(d.roleName),
	}

	result, err := svc.CreateRole(input)
	if err != nil {
		return err
	}

	d.roleArn = *result.Role.Arn

	L.Info("created new role", "arn", d.roleArn)

	aInput := &iam.AttachRolePolicyInput{
		RoleName:  aws.String(d.roleName),
		PolicyArn: aws.String("arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"),
	}

	_, err = svc.AttachRolePolicy(aInput)
	if err != nil {
		return err
	}

	L.Info("attached execution role policy")

	return nil
}

func LambdaCodeSha256(path string) (string, error) {
	sumRaw, err := HashFile(path)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(sumRaw), nil
}

func (d *Deployer) CreateLayer(L hclog.Logger, app *component.Source, info *AppInfo, name, path string) (string, error) {
	sum, err := LambdaCodeSha256(path)
	if err != nil {
		return "", err
	}

	lamSvc := lambda.New(sess)

	list, err := lamSvc.ListLayerVersions(&lambda.ListLayerVersionsInput{
		LayerName: aws.String(name),
	})

	if err == nil {
		for _, ver := range list.LayerVersions {
			info, err := lamSvc.GetLayerVersion(&lambda.GetLayerVersionInput{
				LayerName:     aws.String(name),
				VersionNumber: ver.Version,
			})

			if err != nil {
				return "", err
			}

			if *info.Content.CodeSha256 == sum {
				L.Info("found existing layer", "layer", name, "version", *ver.Version)
				return *info.LayerVersionArn, nil
			}
		}
	}

	s3Svc := s3.New(sess)
	uploader := s3manager.NewUploader(sess)

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}

	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return "", err
	}

	layerName := fmt.Sprintf("%s-%s-%s.zip", app.App, info.BuildId, name)

	L.Info("uploading lib layer", "key", layerName, "size", stat.Size())
	headOut, err := s3Svc.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(d.config.Bucket),
		Key:    aws.String(layerName),
	})

	if err == nil {
		L.Info("reusing existing key", "etag", *headOut.ETag)
	} else {
		_, err = uploader.Upload(&s3manager.UploadInput{
			Body:   f,
			Bucket: aws.String(d.config.Bucket),
			Key:    aws.String(layerName),
		})
		if err != nil {
			return "", nil
		}
	}

	pubOut, err := lamSvc.PublishLayerVersion(&lambda.PublishLayerVersionInput{
		Description:        aws.String(fmt.Sprintf("devflow app %s - %s", app.App, info.BuildId)),
		LayerName:          aws.String(name),
		CompatibleRuntimes: []*string{aws.String(info.Runtime)},
		Content: &lambda.LayerVersionContentInput{
			S3Bucket: aws.String(d.config.Bucket),
			S3Key:    aws.String(layerName),
		},
	})

	if err != nil {
		return "", errors.Wrapf(err, "attempting to publish: %s", path)
	}

	L.Info("published layer", "name", name, "arn", *pubOut.LayerArn, "sha", *pubOut.Content.CodeSha256, "sha-local", sum)

	return *pubOut.LayerVersionArn, nil
}

func (d *Deployer) CreateLibraryLayer(L hclog.Logger, app *component.Source, info *AppInfo, path string) (string, error) {
	return d.CreateLayer(L, app, info, fmt.Sprintf("%s-lib", app.App), path)
}

func (d *Deployer) CreatePreLayer(L hclog.Logger, app *component.Source, info *AppInfo, path string) (string, error) {
	return d.CreateLayer(L, app, info, fmt.Sprintf("%s-pre", app.App), path)
}

func (d *Deployer) CreateFunction(L hclog.Logger, app *component.Source, info *AppInfo) (string, error) {
	lamSvc := lambda.New(sess)

	uploader := s3manager.NewUploader(sess)

	f, err := os.Open(info.AppZip)
	if err != nil {
		return "", err
	}

	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return "", err
	}

	layerName := fmt.Sprintf("%s-%s-app.zip", app.App, info.BuildId)

	L.Info("uploading app", "size", stat.Size(), "bucket", d.config.Bucket, "key", layerName)

	_, err = uploader.Upload(&s3manager.UploadInput{
		Body:   f,
		Bucket: aws.String(d.config.Bucket),
		Key:    aws.String(layerName),
	})
	if err != nil {
		return "", err
	}

	preLayer, err := d.CreatePreLayer(L, app, info, info.PreZip)
	if err != nil {
		return "", err
	}

	libLayer, err := d.CreateLibraryLayer(L, app, info, info.LibZip)
	if err != nil {
		return "", err
	}

	fnInfo, err := lamSvc.GetFunction(&lambda.GetFunctionInput{
		FunctionName: aws.String(app.App),
	})

	var arn string

	if err == nil {
		var newLayers bool

		for _, layer := range fnInfo.Configuration.Layers {
			if !(*layer.Arn == preLayer || *layer.Arn == libLayer) {
				newLayers = true
				break
			}
		}

		if newLayers {
			L.Info("detected layer changes, updating function config")

			_, err := lamSvc.UpdateFunctionConfiguration(&lambda.UpdateFunctionConfigurationInput{
				FunctionName: aws.String(app.App),
				Layers:       []*string{aws.String(preLayer), aws.String(libLayer)},
				Handler:      aws.String("app.handler"),
				Role:         aws.String(d.roleArn),
				Timeout:      aws.Int64(DefaultTimeout),
				MemorySize:   aws.Int64(DefaultMemory),
				Runtime:      aws.String(info.Runtime),
			})

			if err != nil {
				return "", err
			}
		}

		funcCfg, err := lamSvc.UpdateFunctionCode(&lambda.UpdateFunctionCodeInput{
			FunctionName: aws.String(app.App),
			S3Bucket:     aws.String(d.config.Bucket),
			S3Key:        aws.String(layerName),
		})

		if err != nil {
			return "", err
		}

		ver, err := lamSvc.PublishVersion(&lambda.PublishVersionInput{
			CodeSha256:   funcCfg.CodeSha256,
			FunctionName: aws.String(app.App),
		})

		if err != nil {
			return "", err
		}

		arn = *ver.FunctionArn

		L.Info("updated function", "arn", arn, "sha", *funcCfg.CodeSha256)

	} else {
		funcOut, err := lamSvc.CreateFunction(&lambda.CreateFunctionInput{
			Description:  aws.String(fmt.Sprintf("devflow app %s - %s", app.App, info.BuildId)),
			FunctionName: aws.String(app.App),
			Handler:      aws.String("app.handler"),
			Role:         aws.String(d.roleArn),
			Runtime:      aws.String(info.Runtime),
			Layers:       []*string{aws.String(preLayer), aws.String(libLayer)},
			Timeout:      aws.Int64(DefaultTimeout),
			MemorySize:   aws.Int64(DefaultMemory),
			Tags: map[string]*string{
				"devflow.app":    aws.String(app.App),
				"devflow.app.id": aws.String(info.BuildId),
			},
			Code: &lambda.FunctionCode{
				S3Bucket: aws.String(d.config.Bucket),
				S3Key:    aws.String(layerName),
			},
		})

		if err != nil {
			return "", err
		}

		ver, err := lamSvc.PublishVersion(&lambda.PublishVersionInput{
			CodeSha256:   funcOut.CodeSha256,
			FunctionName: aws.String(app.App),
		})

		if err != nil {
			return "", err
		}

		arn = *ver.FunctionArn

		L.Info("created function", "arn", arn, "sha", *funcOut.CodeSha256)
	}

	return arn, nil
}

// MarshalText implements encoding.TextMarshaler so that protobuf generates
// the correct string version.
func (l *LambdaDeployment) MarshalText() ([]byte, error) {
	return []byte(l.FunctionArn), nil
}

func (d *Deployer) Deploy(ctx context.Context, L hclog.Logger, app *component.Source, info *AppInfo) (*LambdaDeployment, error) {
	err := d.SetupRole(L, app)
	if err != nil {
		return nil, err
	}

	arn, err := d.CreateFunction(L, app, info)
	if err != nil {
		return nil, err
	}

	return &LambdaDeployment{FunctionArn: arn}, nil
}

func (d *Deployer) Exec(ctx context.Context, L hclog.Logger, S status.Updater, app *component.Source) error {
	L.Debug("executing lambda app-style environment in ECS", "app", app.App)

	var r runner.Runner

	cfg, err := r.ExtractFromLambda(app.App)
	if err != nil {
		return err
	}

	L.Debug("extracted lambda configuration")

	ecsLauncher := runner.ECSLauncher{}
	cc, err := ecsLauncher.Launch(ctx, L, S, app, cfg)
	if err != nil {
		L.Error("error launching ecs task", "error", err)
		return err
	}

	var (
		fd = os.Stdin.Fd()
		st *terminal.State
	)

	isterm := isatty.IsTerminal(fd)

	if isterm {
		st, err = terminal.MakeRaw(int(fd))
		if err == nil {
			defer terminal.Restore(int(fd), st)
		}
	}

	cc.Exec(S, app.App, "/bin/bash -l")

	terminal.Restore(int(fd), st)

	return nil
}

func (d *Deployer) ConfigSet(ctx context.Context, L hclog.Logger, app *component.Source, cv *component.ConfigVar) error {
	lamSvc := lambda.New(sess)

	fnInfo, err := lamSvc.GetFunction(&lambda.GetFunctionInput{
		FunctionName: aws.String(app.App),
	})
	if err != nil {
		return err
	}

	cur := fnInfo.Configuration

	var envvars map[string]*string

	if cur.Environment != nil {
		envvars = cur.Environment.Variables
		if _, exists := cur.Environment.Variables[cv.Name]; exists {
			L.Warn("Updating config variable", "name", cv.Name)
		} else {
			L.Warn("Setting config variable", "name", cv.Name)
		}
	} else {
		envvars = map[string]*string{}
	}

	var layers []*string

	for _, cl := range cur.Layers {
		layers = append(layers, cl.Arn)
	}

	envvars[cv.Name] = aws.String(cv.Value)

	_, err = lamSvc.UpdateFunctionConfiguration(&lambda.UpdateFunctionConfigurationInput{
		FunctionName: aws.String(app.App),
		Layers:       layers,
		Handler:      cur.Handler,
		Environment: &lambda.Environment{
			Variables: envvars,
		},
		Role:       cur.Role,
		Timeout:    cur.Timeout,
		MemorySize: cur.MemorySize,
		Runtime:    cur.Runtime,
	})

	if err != nil {
		return err
	}

	ver, err := lamSvc.PublishVersion(&lambda.PublishVersionInput{
		FunctionName: aws.String(app.App),
	})

	if err != nil {
		return err
	}

	L.Info("Created new function version", "arn", *ver.FunctionArn)
	return nil
}

func (d *Deployer) ConfigSetFunc() interface{} {
	return d.ConfigSet
}

func (d *Deployer) ConfigGet(ctx context.Context, L hclog.Logger, app *component.Source, cv *component.ConfigVar) error {
	lamSvc := lambda.New(sess)

	fnInfo, err := lamSvc.GetFunction(&lambda.GetFunctionInput{
		FunctionName: aws.String(app.App),
	})
	if err != nil {
		return err
	}

	cur := fnInfo.Configuration

	if val, exists := cur.Environment.Variables[cv.Name]; exists {
		cv.Value = *val
		return nil
	} else {
		return component.ErrNoSuchVariable
	}
}

func (d *Deployer) ConfigGetFunc() interface{} {
	return d.ConfigGet
}

type cloudwatchLogsViewer struct {
	logs      *cloudwatchlogs.CloudWatchLogs
	group     string
	lastToken *string

	stream  *cloudwatchlogs.LogStream
	streams []*cloudwatchlogs.LogStream
}

func (c *cloudwatchLogsViewer) NextLogBatch(ctx context.Context) ([]component.LogEvent, error) {
	for {
		if c.stream == nil {
			if len(c.streams) == 0 {
				return nil, nil
			}
			c.stream = c.streams[0]
			c.streams = c.streams[1:]
			c.lastToken = nil
		}

		output, err := c.logs.GetLogEvents(&cloudwatchlogs.GetLogEventsInput{
			NextToken:     c.lastToken,
			StartFromHead: aws.Bool(true),
			LogGroupName:  aws.String(c.group),
			LogStreamName: c.stream.LogStreamName,
		})

		if err != nil {
			return nil, err
		}

		if len(output.Events) != 0 {
			c.lastToken = output.NextForwardToken

			events := make([]component.LogEvent, len(output.Events))

			for i, ev := range output.Events {
				ms := *ev.Timestamp
				ts := time.Unix(ms/1000, (ms%1000)*1000000)
				msg := strings.TrimRight(*ev.Message, "\n\t")
				events[i] = component.LogEvent{
					Partition: *c.stream.LogStreamName,
					Timestamp: ts,
					Message:   msg,
				}
			}

			return events, nil
		}

		c.stream = nil
	}
}

func (d *Deployer) Logs(ctx context.Context, L hclog.Logger, app *component.Source) (component.LogViewer, error) {
	logs := cloudwatchlogs.New(sess)

	streams, err := logs.DescribeLogStreams(&cloudwatchlogs.DescribeLogStreamsInput{
		LogGroupName: aws.String(fmt.Sprintf("/aws/lambda/%s", app.App)),
		Descending:   aws.Bool(false),
		OrderBy:      aws.String("LastEventTime"),
	})

	if err != nil {
		return nil, err
	}

	return &cloudwatchLogsViewer{
		logs:    logs,
		group:   fmt.Sprintf("/aws/lambda/%s", app.App),
		streams: streams.LogStreams,
	}, nil
}

func (d *Deployer) LogsFunc() interface{} {
	return d.Logs
}

var (
	_ component.Platform     = (*Deployer)(nil)
	_ component.Configurable = (*Deployer)(nil)
)
