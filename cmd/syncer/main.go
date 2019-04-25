package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	kingpin "gopkg.in/alecthomas/kingpin.v2"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/gin-gonic/gin"
	minio "github.com/minio/minio-go"
	log "github.com/sirupsen/logrus"
)

var (
	buildtime  string
	gitcommit  string
	appversion string
)

type Syncer struct {
	BasePath       string
	LocalPath      string
	WebhookURL     string
	WebhookMethod  string
	ListenAddress  string
	BucketName     string
	BucketURL      string
	BucketKey      string
	BucketSecret   string
	BucketLocation string
	BucketPath     string
	UseS3          bool
	Client         *minio.Client
}

func main() {

	svc := new(Syncer)

	app := kingpin.New("syncer", "Transfer files from and to S3 buckets and trigger a webhook.")
	app.Flag("basepath", "Base path for the webapp.").Short('b').Default("/sync").OverrideDefaultFromEnvar("SYNCER_BASEPATH").StringVar(&svc.BasePath)
	app.Flag("storepath", "Path where files are sync to locally.").Short('s').Default("/data").OverrideDefaultFromEnvar("SYNCER_LOCALPATH").StringVar(&svc.LocalPath)
	app.Flag("webhookurl", "Url to be triggered after files are updated.").Short('u').Default("").OverrideDefaultFromEnvar("SYNCER_WEBHOOKURL").StringVar(&svc.WebhookURL)
	app.Flag("webhookmethod", "http method to be used when triggering the webhook").Short('m').Default("POST").OverrideDefaultFromEnvar("SYNCER_WEBHOOKMETHOD").StringVar(&svc.WebhookMethod)
	app.Flag("listenaddress", "Address for syncer to listen on.").Short('l').Default("0.0.0.0:8080").OverrideDefaultFromEnvar("SYNCER_LISTENADDRESS").StringVar(&svc.ListenAddress)
	app.Flag("bucketname", "Name of the S3 bucket").Default("-").OverrideDefaultFromEnvar("SYNCER_BUCKETNAME").StringVar(&svc.BucketName)
	app.Flag("bucketurl", "URL of the S3 bucket").Default("-").OverrideDefaultFromEnvar("SYNCER_BUCKETURL").StringVar(&svc.BucketURL)
	app.Flag("bucketkey", "Access key of the S3 bucket").Default("-").OverrideDefaultFromEnvar("SYNCER_BUCKETKEY").StringVar(&svc.BucketKey)
	app.Flag("bucketsecret", "Secret of the S3 bucket").Default("-").OverrideDefaultFromEnvar("SYNCER_BUCKETSECRET").StringVar(&svc.BucketSecret)
	app.Flag("bucketlocation", "Location of the S3 bucket").Default("-").OverrideDefaultFromEnvar("SYNCER_BUCKETLOCATION").StringVar(&svc.BucketLocation)
	app.Flag("bucketpath", "path inside the bucket").Default("").OverrideDefaultFromEnvar("SYNCER_BUCKETPATH").StringVar(&svc.BucketPath)

	log.SetFormatter(&nested.Formatter{
		HideKeys:        true,
		FieldsOrder:     []string{"component", "category"},
		TimestampFormat: time.RFC3339Nano,
	})

	log.WithField("component", "main").Infof("appversion: %s", appversion)
	log.WithField("component", "main").Infof("gitcommit:  %s", gitcommit)
	log.WithField("component", "main").Infof("buildtime:  %s", buildtime)

	kingpin.MustParse(app.Parse(os.Args[1:]))
	err := svc.Init()
	if err != nil {
		log.Println(err.Error())
		os.Exit(1)
	}
	if err := svc.SyncFromBucket(); err != nil {
		log.Println(err.Error())
		os.Exit(1)
	}
	//os.Exit(0)

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(GinLogger())

	router.PUT(svc.BasePath+"/*filename", svc.handleUpload)
	router.DELETE(svc.BasePath+"/*filename", svc.handleDelete)
	router.GET(svc.BasePath+"/*filename", svc.handleDownload)
	router.POST(svc.BasePath, svc.handleSync)
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

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server Shutdown:", err)
	}

	<-ctx.Done()
	log.Println("Shutdown")

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

	return nil
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

func (svc *Syncer) getFromBucket(objectKey string) error {
	err := svc.Client.FGetObject(svc.BucketName, objectKey, svc.LocalPath+"/"+objectKey, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("Failed to download object %s: %s", objectKey, err.Error())
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
	doneCh := make(chan struct{})
	defer close(doneCh)
	isRecursive := true
	objectCh := svc.Client.ListObjects(svc.BucketName, svc.BucketPath, isRecursive, doneCh)

	for object := range objectCh {
		log.WithField("component", "remote").Debugf("object: %#v", object)
		if object.Err != nil {
			return object.Err
		}

		err := svc.createFolder(object.Key)
		if err != nil {
			log.WithField("component", "local").Error(err.Error())
		}

		log.WithField("component", "remote").Infof("get object %s", object.Key)
		err = svc.getFromBucket(object.Key)
		if err != nil {
			log.WithField("component", "remote").Error(err.Error())
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
		return
	}

	err = svc.createFolder(filename)
	if err != nil {
		log.WithField("component", "local").Error(err.Error())
		c.String(http.StatusInternalServerError, err.Error())
		return
	}

	log.WithField("component", "remote").Infof("get object %s", filename)
	err = svc.getFromBucket(filename)
	if err != nil {
		log.WithField("component", "remote").Error(err.Error())
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
}

func (svc *Syncer) handleSync(c *gin.Context) {
	err := svc.SyncFromBucket()
	if err != nil {
		c.String(http.StatusInternalServerError, "Failed to sync files from bucket: %s", err.Error())
		return
	}
	err = svc.triggerWebhook()
	if err != nil {
		c.String(http.StatusInternalServerError, "Failed to trigger webhook: %s %s", svc.WebhookMethod, svc.WebhookURL)
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
	log.WithField("component", "remote").Infof("Uploading %s to %s", filename, dirname+filename)
	count, err := svc.Client.PutObject(svc.BucketName, dirname+filename, c.Request.Body, c.Request.ContentLength, minio.PutObjectOptions{
		ContentType: c.ContentType(),
	})
	if err != nil {
		msg := fmt.Sprintf("Failed to save file %s: %s", dirname+filename, err.Error())
		log.WithField("component", "remote").Error(msg)
		c.String(500, msg)
		return
	}

	if count != c.Request.ContentLength {
		log.WithField("component", "remote").Errorf("Written bytes are not equal: read: %d wrote: %d", count, c.Request.ContentLength)
		c.String(500, "Error in file transmission")
		return
	}

	log.WithField("component", "remote").Infof("get object %s", dirname+filename)
	err = svc.getFromBucket(dirname + filename)
	if err != nil {
		c.String(500, "Error sync back from bucket: %s", err.Error())
		return
	}

	err = svc.triggerWebhook()
	if err != nil {

		c.String(http.StatusInternalServerError, "Failed to trigger webhook: %s %s", svc.WebhookMethod, svc.WebhookURL)
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
		return
	}

	localdirname := ""
	if len(svc.BucketPath) > 0 {
		localdirname = svc.LocalPath + "/" + svc.BucketPath
	} else {
		localdirname = svc.LocalPath
	}

	info, err := os.Stat(localdirname + "/" + filename)
	if err != nil && os.IsNotExist(err) {
		msg := fmt.Sprintf("The file %s does not exist.", filename)
		log.WithField("component", "local").Error(msg)
		c.String(404, msg)
		return
	}
	log.WithField("component", "local").Infof("removing local file: %s", filename)
	err = os.RemoveAll(localdirname + "/" + filename)
	if err != nil {
		msg := fmt.Sprintf("Failed delete file %s: %s", filename, err.Error())
		log.WithField("component", "local").Error(msg)
		c.String(http.StatusInternalServerError, msg)
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
			msg := fmt.Sprintf("Failed to trigger webhook: %s %s", svc.WebhookMethod, svc.WebhookURL)
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
