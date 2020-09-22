package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	kingpin "gopkg.in/alecthomas/kingpin.v2"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/gin-gonic/gin"
	minio "github.com/minio/minio-go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

var (
	buildtime  string
	gitcommit  string
	appversion string
	logoutput  io.Writer
)

type Syncer struct {
	BasePath           string
	LocalPath          string
	PurgeMissing       bool
	StaticMap          string
	WebhookURL         string
	WebhookMethod      string
	PreSyncCmdString   string
	ListenAddress      string
	BucketName         string
	BucketURL          string
	BucketKey          string
	BucketSecret       string
	BucketLocation     string
	BucketPath         string
	Lock               sync.Mutex
	UseS3              bool
	PullIntervalString string
	RunPeriodicSync    bool
	TempDir            string
	LogLevel           string
	Client             *minio.Client
}

func main() {

	svc := new(Syncer)

	app := kingpin.New("syncer", "Transfer files from and to S3 buckets and trigger a webhook.")
	app.Version(fmt.Sprintf("syncer %s-%s %s", appversion, gitcommit, buildtime))
	app.Flag("web.root", "Base path for the webapp.").Short('b').Default("/sync").Envar("SYNCER_BASEPATH").StringVar(&svc.BasePath)
	app.Flag("fs.localpath", "Path where files are sync to locally.").Short('s').Default("/data").Envar("SYNCER_LOCALPATH").StringVar(&svc.LocalPath)
	app.Flag("fs.purge.missing", "Remove local files which are not present on the bucket.").Envar("SYNCER_PURGEMISSING").BoolVar(&svc.PurgeMissing)
	app.Flag("webhookurl", "Url to be triggered after files are updated.").Short('u').Default("").Envar("SYNCER_WEBHOOKURL").StringVar(&svc.WebhookURL)
	app.Flag("webhookmethod", "http method to be used when triggering the webhook").Short('m').Default("POST").Envar("SYNCER_WEBHOOKMETHOD").StringVar(&svc.WebhookMethod)
	app.Flag("presynccmd", "Command to be executed before the files are uploaded").Short('p').Default("").Envar("SYNCER_PRESYNCCMD").StringVar(&svc.PreSyncCmdString)
	app.Flag("web.listenaddress", "Address for syncer to listen on.").Short('l').Default("0.0.0.0:8080").Envar("SYNCER_LISTENADDRESS").StringVar(&svc.ListenAddress)
	app.Flag("bucket.name", "Name of the S3 bucket").Default("-").Envar("SYNCER_BUCKETNAME").StringVar(&svc.BucketName)
	app.Flag("bucket.url", "URL of the S3 bucket").Default("-").Envar("SYNCER_BUCKETURL").StringVar(&svc.BucketURL)
	app.Flag("bucket.key", "Access key of the S3 bucket").Default("-").Envar("SYNCER_BUCKETKEY").StringVar(&svc.BucketKey)
	app.Flag("bucket.secret", "Secret of the S3 bucket").Default("-").Envar("SYNCER_BUCKETSECRET").StringVar(&svc.BucketSecret)
	app.Flag("bucket.location", "Location of the S3 bucket").Default("-").Envar("SYNCER_BUCKETLOCATION").StringVar(&svc.BucketLocation)
	app.Flag("bucket.path", "path inside the bucket").Default("").Envar("SYNCER_BUCKETPATH").StringVar(&svc.BucketPath)
	app.Flag("pullinterval", "Interval in seconds to check and sync updates on the s3 endpoint").Default("30s").Envar("SYNCER_PULLINTERVAL").StringVar(&svc.PullIntervalString)
	app.Flag("fs.tempdir", "Temp dir where files are store for the presynccmd to run on.").Default("/tmp").Envar("SYNCER_TEMPDIR").StringVar(&svc.TempDir)
	app.Flag("loglevel", "Log level (info,warning,debug,error,trace)").Default("info").Envar("SYNCER_LOGLEVEL").StringVar(&svc.LogLevel)
	kingpin.MustParse(app.Parse(os.Args[1:]))

	log.SetReportCaller(true)

	log.SetFormatter(&nested.Formatter{
		HideKeys:        true,
		FieldsOrder:     []string{"component", "category"},
		TimestampFormat: time.RFC3339Nano,
		NoColors:        true,
	})

	switch svc.LogLevel {
	case "info":
		log.SetLevel(log.InfoLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	case "warning":
		log.SetLevel(log.WarnLevel)
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "trace":
		log.SetLevel(log.TraceLevel)
	}

	log.WithField("component", "main").Infof("==== Server startup ====")
	log.WithField("component", "main").Infof("appversion: %s", appversion)
	log.WithField("component", "main").Infof("gitcommit:  %s", gitcommit)
	log.WithField("component", "main").Infof("buildtime:  %s", buildtime)

	err := svc.Init()
	if err != nil {
		log.Println(err.Error())
		os.Exit(1)
	}

	fields := reflect.TypeOf(*svc)
	values := reflect.ValueOf(*svc)

	num := fields.NumField()

	for i := 0; i < num; i++ {
		field := fields.Field(i)
		value := values.Field(i)
		//fmt.Print("Type:", field.Type, ",", field.Name, "=", value, "\n")
		if field.Name == "BucketSecret" || field.Name == "BucketKey" {
			log.WithField("component", "config").Infof("%s:  %v", field.Name, "******")
			continue
		}
		log.WithField("component", "config").Infof("%s:  %v", field.Name, value)
	}

	if err := svc.SyncFromBucket(); err != nil {
		log.Println(err.Error())
		os.Exit(1)
	}
	//os.Exit(0)

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	health := router.Group("/admin/healthz")
	health.GET("", func(c *gin.Context) { c.String(200, "healthy") })

	metrics := router.Group("/admin/metrics")
	metrics.GET("", prometheusHandler())

	sync := router.Group(svc.BasePath)
	sync.Use(GinLogger())
	sync.PUT("/*filename", svc.handleUpload)
	sync.DELETE("/*filename", svc.handleDelete)
	sync.GET("/*filename", svc.handleDownload)
	sync.POST("", svc.handleSync)
	router.StaticFS("/objects", gin.Dir(svc.LocalPath, true))

	srv := &http.Server{
		Addr:    svc.ListenAddress,
		Handler: router,
	}

	go func() {
		// service connections
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	quit := make(chan os.Signal)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server ...")
	svc.RunPeriodicSync = false

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server Shutdown:", err)
	}

	<-ctx.Done()
	log.Println("Shutdown")

}

func prometheusHandler() gin.HandlerFunc {
	h := promhttp.Handler()
	return func(c *gin.Context) {
		h.ServeHTTP(c.Writer, c.Request)
	}
}

func (svc *Syncer) Init() error {

	_, err := os.Stat(svc.LocalPath)
	if err != nil && os.IsNotExist(err) {
		if err := os.MkdirAll(svc.LocalPath, 0755); err != nil {
			return err
		}
	}

	if svc.BucketURL == "-" {
		svc.UseS3 = false
		return nil
	}

	client, err := minio.New(
		svc.BucketURL,
		svc.BucketKey,
		svc.BucketSecret,
		true)

	if err != nil {
		return fmt.Errorf("Cannot create client: %s", err.Error())
	}
	svc.Client = client
	ok, err := svc.Client.BucketExists(svc.BucketName)
	if err != nil {
		return fmt.Errorf("Cannot check bucket existence: %s", err.Error())
	}

	if !ok {
		if err := svc.Client.MakeBucket(svc.BucketName, svc.BucketLocation); err != nil {
			return fmt.Errorf("Cannot create bucket.")
		}
	}

	if len(svc.PullIntervalString) > 0 {
		interval, err := time.ParseDuration(svc.PullIntervalString)
		if err != nil {
			return err
		}
		svc.RunPeriodicSync = true
		go svc.PrediodicSync(interval)
	}

	return nil
}

func (svc *Syncer) PrediodicSync(interval time.Duration) {
	lastSync := time.Now()
	loglevel := log.GetLevel()
	for {
		if time.Since(lastSync) > interval {
			log.WithField("component", "periodicsync").Debugf("sync from bucket")
			if loglevel != log.DebugLevel {
				log.SetLevel(log.ErrorLevel)
			}

			if err := svc.SyncFromBucket(); err != nil {
				log.WithField("component", "periodicsync").Errorf("failed to sync from bucket: %s", err.Error())
			}
			log.SetLevel(loglevel)
			lastSync = time.Now()
		}
		time.Sleep(500 * time.Millisecond)
		if !svc.RunPeriodicSync {
			return
		}
	}

}

func (svc *Syncer) createFolder(objectKey string) error {
	dirname := filepath.Dir(objectKey)
	localpath := svc.LocalPath + "/" + dirname
	_, err := os.Stat(localpath)
	if err != nil && os.IsNotExist(err) {
		log.WithField("component", "local").Debugf("creating folder %s", localpath)
		err := os.MkdirAll(localpath, 0755)
		if err != nil {
			return fmt.Errorf("Failed to create folder %s: %s", dirname, err.Error())
		}
	}
	return nil
}

func (svc *Syncer) getFromBucket(bucketObjectKey string) error {

	localObjectName := strings.TrimPrefix(bucketObjectKey, svc.BucketPath+"/")

	if err := svc.createFolder(localObjectName); err != nil {
		return err
	}

	log.WithField("component", "remote").Infof("storing %q to %q", bucketObjectKey, svc.LocalPath+"/"+localObjectName)
	err := svc.Client.FGetObject(svc.BucketName, bucketObjectKey, svc.LocalPath+"/"+localObjectName, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("Failed to download object %s: %s", bucketObjectKey, err.Error())
	}
	return nil
}

func (svc *Syncer) getObjectList(prefix string) []string {
	doneCh := make(chan struct{})
	defer close(doneCh)
	isRecursive := true
	objectCh := svc.Client.ListObjects(svc.BucketName, prefix, isRecursive, doneCh)
	var objectlist []string
	for object := range objectCh {
		objectlist = append(objectlist, object.Key)
	}

	return objectlist
}

func (svc *Syncer) SyncFromBucket() error {
	svc.Lock.Lock()
	defer svc.Lock.Unlock()

	doneCh := make(chan struct{})
	defer close(doneCh)
	isRecursive := true
	objectCh := svc.Client.ListObjects(svc.BucketName, svc.BucketPath, isRecursive, doneCh)

	// check the remove objects against the local file
	objcounter := 0
	for object := range objectCh {
		needSync := false
		log.WithField("component", "remote").Tracef("object: %#v", object)
		if object.Err != nil {
			return object.Err
		}

		localObjectName := strings.TrimPrefix(object.Key, svc.BucketPath+"/")
		info, _ := svc.Client.StatObject(svc.BucketName, object.Key, minio.StatObjectOptions{})
		localobject, err := os.Stat(svc.LocalPath + "/" + localObjectName)

		if os.IsNotExist(err) {
			needSync = true
		}

		if err == nil {
			if info.LastModified.UTC().After(localobject.ModTime().UTC()) {
				needSync = true
			}
		}

		if needSync {
			log.WithField("component", "remote").Infof("object needs sync: %s", object.Key)
			log.WithField("component", "remote").Infof("get object %s", object.Key)
			err = svc.getFromBucket(object.Key)
			if err != nil {
				log.WithField("component", "remote").Error(err.Error())
			}
			objcounter = objcounter + 1
		} else {
			log.WithField("component", "remote").Debugf("object is in sync: %s", object.Key)
		}

	}

	// check if the local files exist in the bucket
	err := filepath.Walk(svc.LocalPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		remoteObjectName := strings.TrimPrefix(path, svc.LocalPath+"/")
		remoteObject, err := svc.Client.StatObject(svc.BucketName, svc.BucketPath+"/"+remoteObjectName, minio.StatObjectOptions{})
		log.WithField("component", "local").Debugf("check local object for existence in bucket: %s <-> %s", remoteObjectName, remoteObject.Key)
		if err != nil {
			if minioErr, ok := err.(minio.ErrorResponse); ok {
				if minioErr.StatusCode == 404 {
					log.WithField("component", "local").Infof("remote object does not exist: %s", remoteObjectName)
					if svc.PurgeMissing {
						log.WithField("component", "local").Infof("removing object: %s", path)
						if removeErr := os.Remove(path); removeErr != nil {
							log.WithField("component", "local").Errorf("failed to remove object %s: %s", path, removeErr.Error())
							return nil
						}
						objcounter = objcounter + 1
					}
					return nil
				}
			}
			log.WithField("component", "local").Errorf("checking remote object error: %s", err.Error())
		}

		return nil
	})

	if err != nil {
		log.WithField("component", "local").Errorf("Walking local data dir error: %s", err.Error())
	}

	if objcounter > 0 {
		err := svc.triggerWebhook()
		if err != nil {
			return err
		}
	}

	return nil
}

func (svc *Syncer) handleDownload(c *gin.Context) {
	filename := c.Param("filename")
	filename = strings.TrimPrefix(filename, "/")

	_, err := svc.Client.StatObject(svc.BucketName, filename, minio.StatObjectOptions{})
	if err != nil {
		log.WithField("component", "remote").Error(err.Error())
		c.String(http.StatusNotFound, err.Error())
		c.Abort()
		return
	}

	err = svc.createFolder(filename)
	if err != nil {
		log.WithField("component", "local").Error(err.Error())
		c.String(http.StatusInternalServerError, err.Error())
		c.Abort()
		return
	}

	log.WithField("component", "remote").Infof("get object %s", filename)
	err = svc.getFromBucket(filename)
	if err != nil {
		log.WithField("component", "remote").Error(err.Error())
		c.String(http.StatusInternalServerError, err.Error())
		c.Abort()
		return
	}
}

func (svc *Syncer) handleSync(c *gin.Context) {
	err := svc.SyncFromBucket()
	if err != nil {
		c.String(http.StatusInternalServerError, "Failed to sync files from bucket: %s", err.Error())
		c.Abort()
		return
	}
	c.String(http.StatusOK, "Files are synced.")
}

func (svc *Syncer) handleUpload(c *gin.Context) {
	filename := c.Param("filename")
	dirname := ""
	filename = strings.TrimPrefix(filename, "/")
	if len(svc.BucketPath) > 0 {
		dirname = svc.BucketPath + "/"
	}

	filecontent, err := ioutil.ReadAll(c.Request.Body)
	if err != nil {
		msg := fmt.Sprintf("Failed to read bytes from body: %s: %s", dirname+filename, err.Error())
		log.WithField("component", "local").Error(msg)
		c.String(500, msg)
		c.Abort()
		return
	}
	r := bytes.NewReader(filecontent)

	if len(svc.PreSyncCmdString) > 0 {
		// we save the file temporarily
		tempdir := filepath.Dir(filename)
		if tempdir != "." {
			if err := os.MkdirAll(svc.TempDir+"/"+tempdir, 0777); err != nil {
				msg := fmt.Errorf("failed to create tmpdir: %s", err.Error())
				log.WithField("component", "local").Errorf(msg.Error())
				c.String(500, msg.Error())
				c.Abort()
				return
			}
		}
		tempfile, err := ioutil.TempFile(svc.TempDir+"/"+tempdir, filepath.Base(filename)+"-*")
		defer os.Remove(tempfile.Name())
		if err != nil {
			msg := fmt.Errorf("Failed to create temp file %s: %s", dirname+filename, err.Error())
			log.WithField("component", "local").Error(msg.Error())
			c.String(500, msg.Error())
			c.Abort()
			return
		}

		_, err = tempfile.Write(filecontent)
		if err != nil {
			msg := fmt.Errorf("Failed to write content to temp file %s: %s", tempfile.Name(), err.Error())
			log.WithField("component", "local").Error(msg.Error())
			c.String(500, msg.Error())
			c.Abort()
			return
		}
		log.WithField("component", "presynccmd").Infof("Running presynccmd %s %s", svc.PreSyncCmdString, tempfile.Name())

		parts := strings.Split(svc.PreSyncCmdString, " ")
		cmd := exec.Command(parts[0], parts[1:]...)

		cmd.Args = append(cmd.Args, tempfile.Name())
		cmdOutput, err := cmd.Output()
		if err != nil {
			if _, ok := err.(*exec.ExitError); !ok {
				msg := fmt.Errorf("Presynccmd failed: %s: %s", tempfile.Name(), err.Error())
				log.WithField("component", "presynccmd").Error(msg.Error())
				c.String(500, msg.Error())
				c.Abort()
				return
			}
			msg := fmt.Errorf("Presynccmd failed: %s: %s %s", tempfile.Name(), err.Error(), string(err.(*exec.ExitError).Stderr))
			log.WithField("component", "presynccmd").Error(msg.Error())
			c.String(412, msg.Error())
			c.Abort()
			return
		}
		for _, line := range strings.Split(string(cmdOutput), "\n") {
			if len(line) > 0 {
				log.WithField("component", "presynccmd").Infof("Result %s: %s", tempfile.Name(), line)
			}
		}

	}

	log.WithField("component", "remote").Infof("Uploading %s to %s", filename, dirname+filename)
	count, err := svc.Client.PutObject(svc.BucketName, dirname+filename, r, c.Request.ContentLength, minio.PutObjectOptions{
		ContentType: c.ContentType(),
	})
	if err != nil {
		msg := fmt.Sprintf("Failed to save file %s: %s", dirname+filename, err.Error())
		log.WithField("component", "remote").Error(msg)
		c.String(500, msg)
		c.Abort()
		return
	}

	if count != c.Request.ContentLength {
		log.WithField("component", "remote").Errorf("Written bytes are not equal: read: %d wrote: %d", count, c.Request.ContentLength)
		c.String(500, "Error in file transmission")
		c.Abort()
		return
	}

	if err := svc.SyncFromBucket(); err != nil {
		log.WithField("component", "local").Errorf("failed to sync from bucket: %s", err.Error())
		c.String(500, "Error sync back from bucket: %s", err.Error())
		c.Abort()
		return

	}

	c.String(http.StatusCreated, fmt.Sprintf("Created: %s", filename))
}

func (svc *Syncer) handleDelete(c *gin.Context) {
	filename := c.Param("filename")
	if strings.Contains(filename, "../") {
		msg := fmt.Sprintf("This is not allowed. Go away.")
		log.WithField("component", "local").Error(msg)
		c.String(http.StatusForbidden, msg)
		c.Abort()
		return
	}

	info, err := os.Stat(svc.LocalPath + "/" + filename)
	if err != nil && os.IsNotExist(err) {
		msg := fmt.Sprintf("The file %s does not exist.", filename)
		log.WithField("component", "local").Error(msg)
		c.String(404, msg)
		c.Abort()
		return
	}
	log.WithField("component", "local").Infof("removing local file: %s", filename)
	err = os.RemoveAll(svc.LocalPath + "/" + filename)
	if err != nil {
		msg := fmt.Sprintf("Failed delete file %s: %s", filename, err.Error())
		log.WithField("component", "local").Error(msg)
		c.String(http.StatusInternalServerError, msg)
		c.Abort()
		return
	}

	filename = strings.TrimPrefix(filename, "/")
	bucketdirname := ""
	if len(svc.BucketPath) > 0 {
		bucketdirname = svc.BucketPath + "/"
	}

	if info.IsDir() {
		// get a list of all files in the bucket with directory as prefix
		objectlist := svc.getObjectList(bucketdirname + filename)
		for _, o := range objectlist {
			err = svc.Client.RemoveObject(svc.BucketName, o)
			if err != nil {
				msg := fmt.Sprintf("Failed to remove file from bucket %s: %s", o, err.Error())
				log.WithField("component", "remote").Error(msg)
				c.String(http.StatusInternalServerError, msg)
				c.Abort()
				return
			}
			log.WithField("component", "remote").Infof("removed object from bucket: %s", o)
		}

		c.String(http.StatusOK, "removed %d objects from bucket", len(objectlist))
		return
	}

	log.WithField("component", "remote").Infof("removing file from bucket: %s", bucketdirname+filename)
	err = svc.Client.RemoveObject(svc.BucketName, bucketdirname+filename)
	if err != nil {
		msg := fmt.Sprintf("Failed to remove file from bucket %s: %s", bucketdirname+filename, err.Error())
		log.WithField("component", "remote").Error(msg)
		c.String(http.StatusInternalServerError, msg)
		return
	}
	err = svc.triggerWebhook()
	if err != nil {
		c.String(http.StatusInternalServerError, "Failed to trigger webhook: %s %s", svc.WebhookMethod, svc.WebhookURL)
		c.Abort()
		return
	}
	c.String(http.StatusOK, fmt.Sprintf("Deleted: %s", filename))
}

func (svc *Syncer) triggerWebhook() error {
	if len(svc.WebhookURL) > 0 {
		log.WithField("component", "trigger").Infof("Triggering webhook: %s %s", svc.WebhookMethod, svc.WebhookURL)
		c := &http.Client{}
		r, _ := http.NewRequest(svc.WebhookMethod, svc.WebhookURL, nil)
		_, err := c.Do(r)
		if err != nil {
			msg := fmt.Sprintf("Failed to trigger webhook: %s %s (%s)", svc.WebhookMethod, svc.WebhookURL, err.Error())
			log.WithField("component", "trigger").Error(msg)
			return err
		}
	}
	return nil
}

func GinLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		t := time.Now()
		c.Next()

		// after request
		latency := time.Since(t)

		// access the status we are sending
		status := c.Writer.Status()
		logstring := fmt.Sprintf("%s - %d - %s - %s (%s)",
			c.Request.RemoteAddr,
			status,
			c.Request.Method,
			c.Request.RequestURI,
			latency)

		log.WithField("component", "gin").Infoln(logstring)

	}
}
