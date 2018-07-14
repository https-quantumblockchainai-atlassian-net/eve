// Copyright (c) 2017 Zededa, Inc.
// All rights reserved.

package zedagent

import (
	"errors"
	"fmt"
	"github.com/zededa/go-provision/cast"
	"github.com/zededa/go-provision/pubsub"
	"github.com/zededa/go-provision/types"
	"log"
	"os"
	"time"
)

func lookupDownloaderConfig(ctx *zedagentContext, objType string,
	safename string) *types.DownloaderConfig {

	pub := downloaderPublication(ctx, objType)
	c, _ := pub.Get(safename)
	if c == nil {
		log.Printf("lookupDownloaderConfig(%s/%s) not found\n",
			objType, safename)
		return nil
	}
	config := cast.CastDownloaderConfig(c)
	if config.Key() != safename {
		log.Printf("lookupDownloaderConfig(%s) got %s; ignored %+v\n",
			safename, config.Key(), config)
		return nil
	}
	return &config
}

func createDownloaderConfig(ctx *zedagentContext, objType string,
	safename string, sc *types.StorageConfig) {

	log.Printf("createDownloaderConfig(%s/%s)\n", objType, safename)

	if m := lookupDownloaderConfig(ctx, objType, safename); m != nil {
		m.RefCount += 1
		publishDownloaderConfig(ctx, objType, m)
	} else {
		log.Printf("createDownloaderConfig(%s) add\n", safename)
		n := types.DownloaderConfig{
			Safename:        safename,
			DownloadURL:     sc.DownloadURL,
			UseFreeUplinks:  false,
			Size:            sc.Size,
			TransportMethod: sc.TransportMethod,
			Dpath:           sc.Dpath,
			ApiKey:          sc.ApiKey,
			Password:        sc.Password,
			ImageSha256:     sc.ImageSha256,
			RefCount:        1,
		}
		publishDownloaderConfig(ctx, objType, &n)
	}
	log.Printf("createDownloaderConfig(%s/%s) done\n", objType, safename)
}

func updateDownloaderStatus(ctx *zedagentContext,
	status *types.DownloaderStatus) {

	key := status.Key()
	objType := status.ObjType
	log.Printf("updateDownloaderStatus(%s/%s) to %v\n",
		objType, key, status.State)

	// Ignore if any Pending* flag is set
	if status.PendingAdd || status.PendingModify || status.PendingDelete {
		log.Printf("updateDownloaderStatus for %s, Skipping due to Pending*\n", key)
		return
	}

	switch objType {
	case baseOsObj:
		baseOsHandleStatusUpdateSafename(ctx, status.Safename)

	case certObj:
		certObjHandleStatusUpdateSafename(ctx, status.Safename)

	default:
		log.Fatalf("updateDownloaderStatus for %s, unsupported objType <%s>\n",
			key, objType)
		return
	}
	log.Printf("updateDownloaderStatus(%s/%s) done\n",
		objType, key)
}

// Lookup published config;
func removeDownloaderConfig(ctx *zedagentContext, objType string, safename string) {

	log.Printf("removeDownloaderConfig(%s/%s)\n", objType, safename)

	config := lookupDownloaderConfig(ctx, objType, safename)
	if config == nil {
		log.Printf("removeDownloaderConfig(%s/%s) no Config\n",
			objType, safename)
		return
	}

	if config.RefCount > 1 {
		log.Printf("removeDownloaderConfig(%s/%s) decrementing refCount %d\n",
			objType, safename, config.RefCount)
		config.RefCount -= 1
		publishDownloaderConfig(ctx, objType, config)
		return
	}
	unpublishDownloaderConfig(ctx, objType, config)
	log.Printf("removeDownloaderConfig(%s/%s) done\n", objType, safename)
}

func lookupDownloaderStatus(ctx *zedagentContext, objType string,
	safename string) *types.DownloaderStatus {

	sub := downloaderSubscription(ctx, objType)
	c, _ := sub.Get(safename)
	if c == nil {
		log.Printf("lookupDownloaderStatus(%s/%s) not found\n",
			objType, safename)
		return nil
	}
	status := cast.CastDownloaderStatus(c)
	if status.Key() != safename {
		log.Printf("lookupDownloaderStatus(%s) got %s; ignored %+v\n",
			safename, status.Key(), status)
		return nil
	}
	return &status
}

func checkStorageDownloadStatus(ctx *zedagentContext,
	objType string, uuidStr string,
	config []types.StorageConfig, status []types.StorageStatus) *types.RetStatus {

	ret := &types.RetStatus{}
	log.Printf("checkStorageDownloadStatus for %s\n", uuidStr)

	ret.Changed = false
	ret.AllErrors = ""
	ret.MinState = types.MAXSTATE
	ret.WaitingForCerts = false

	for i, sc := range config {

		ss := &status[i]

		safename := types.UrlToSafename(sc.DownloadURL, sc.ImageSha256)

		log.Printf("checkStorageDownloadStatus %s, image status %v\n", safename, ss.State)
		if ss.State == types.INSTALLED {
			ret.MinState = ss.State
			log.Printf("checkStorageDownloadStatus %s,is already installed\n", safename)
			continue
		}

		if sc.ImageSha256 != "" {
			// Shortcut if image is already verified
			vs := lookupVerificationStatusAny(ctx, objType,
				safename, sc.ImageSha256)

			if vs != nil && vs.State == types.DELIVERED {
				log.Printf(" %s, exists verified with sha %s\n",
					safename, sc.ImageSha256)
				if vs.Safename != safename {
					// If found based on sha256
					log.Printf("found diff safename %s\n",
						vs.Safename)
				}
				// If we don't already have a RefCount add one
				if !ss.HasVerifierRef {
					log.Printf("checkStorageDownloadStatus %s, !HasVerifierRef\n", vs.Safename)
					createVerifierConfig(ctx, objType,
						vs.Safename, &sc, false)
					ss.HasVerifierRef = true
					ret.Changed = true
				}
				if ret.MinState > vs.State {
					ret.MinState = vs.State
				}
				if vs.State != ss.State {
					log.Printf("checkStorageDownloadStatus(%s) from vs set ss.State %d\n",
						safename, vs.State)
					ss.State = vs.State
					ret.Changed = true
				}
				continue
			}
		}

		if !ss.HasDownloaderRef {
			log.Printf("checkStorageDownloadStatus %s, !HasDownloaderRef\n", safename)
			createDownloaderConfig(ctx, objType, safename, &sc)
			ss.HasDownloaderRef = true
			ret.Changed = true
		}

		ds := lookupDownloaderStatus(ctx, objType, safename)
		if ds == nil {
			log.Printf("LookupDownloaderStatus %s not yet\n",
				safename)
			ret.MinState = types.DOWNLOAD_STARTED
			continue
		}

		if ret.MinState > ds.State {
			ret.MinState = ds.State
		}
		if ds.State != ss.State {
			log.Printf("checkStorageDownloadStatus(%s) from ds set ss.State %d\n",
				safename, ds.State)
			ss.State = ds.State
			ret.Changed = true
		}

		switch ss.State {
		case types.INITIAL:
			log.Printf("checkStorageDownloadStatus %s, downloader error, %s\n",
				uuidStr, ds.LastErr)
			ss.Error = ds.LastErr
			ret.AllErrors = appendError(ret.AllErrors, "downloader",
				ds.LastErr)
			ss.ErrorTime = ds.LastErrTime
			ret.ErrorTime = ss.ErrorTime
			ret.Changed = true
		case types.DOWNLOAD_STARTED:
			// Nothing to do
		case types.DOWNLOADED:

			log.Printf("checkStorageDownloadStatus %s, is downloaded\n", safename)
			// if verification is needed
			if sc.ImageSha256 != "" {
				// start verifier for this object
				if !ss.HasVerifierRef {
					err := createVerifierConfig(ctx,
						objType, safename, &sc, true)
					if err == nil {
						ss.HasVerifierRef = true
						ret.Changed = true
					} else {
						ret.WaitingForCerts = true
					}
				}
			}
		}
	}

	if ret.MinState == types.MAXSTATE {
		ret.MinState = types.DOWNLOADED
	}

	return ret
}

func installDownloadedObjects(objType string, uuidStr string,
	config []types.StorageConfig, status []types.StorageStatus) bool {

	ret := true
	log.Printf("installDownloadedObjects(%s)\n", uuidStr)

	for i, sc := range config {

		ss := &status[i]

		safename := types.UrlToSafename(sc.DownloadURL, sc.ImageSha256)

		installDownloadedObject(objType, safename, sc, ss)

		// if something is still not installed, mark accordingly
		if ss.State != types.INSTALLED {
			ret = false
		}
	}

	log.Printf("installDownloadedObjects(%s) done %v\n", uuidStr, ret)
	return ret
}

// based on download/verification state, if
// the final installation directory is mentioned,
// move the object there
func installDownloadedObject(objType string, safename string,
	config types.StorageConfig, status *types.StorageStatus) error {

	var ret error
	var srcFilename string = objectDownloadDirname + "/" + objType

	log.Printf("installDownloadedObject(%s/%s, %v)\n", objType, safename, status.State)

	// if the object is in downloaded state,
	// pick from pending directory
	// if ithe object is n delivered state,
	//  pick from verified directory
	switch status.State {

	case types.INSTALLED:
		log.Printf("installDownloadedObject %s, already installed\n",
			safename)
		return nil

	case types.DOWNLOADED:
		if config.ImageSha256 != "" {
			log.Printf("installDownloadedObject %s, verification pending\n",
				safename)
			return nil
		}
		srcFilename += "/pending/" + safename
		break

	case types.DELIVERED:
		srcFilename += "/verified/" + config.ImageSha256 + "/" +
			types.SafenameToFilename(safename)
		break

		// XXX do we need to handle types.INITIAL for failures?
	default:
		log.Printf("installDownloadedObject %s, still not ready (%d)\n",
			safename, status.State)
		return nil
	}

	// ensure the file is present
	if _, err := os.Stat(srcFilename); err != nil {
		log.Fatal(err)
	}

	// move to final installation point
	if config.FinalObjDir != "" {

		var dstFilename string = config.FinalObjDir

		switch objType {
		case certObj:
			ret = installCertObject(srcFilename, dstFilename, safename)

		case baseOsObj:
			ret = installBaseOsObject(srcFilename, dstFilename)

		default:
			errStr := fmt.Sprintf("installDownloadedObject %s, Unsupported Object Type %v",
				safename, objType)
			log.Println(errStr)
			ret = errors.New(status.Error)
		}
	} else {
		errStr := fmt.Sprintf("installDownloadedObject %s, final dir not set %v\n", safename, objType)
		log.Println(errStr)
		ret = errors.New(errStr)
	}

	if ret == nil {
		status.State = types.INSTALLED
		log.Printf("installDownloadedObject(%s) done\n", safename)
	} else {
		status.State = types.INITIAL
		status.Error = fmt.Sprintf("%s", ret)
		status.ErrorTime = time.Now()
	}
	return ret
}

func publishDownloaderConfig(ctx *zedagentContext, objType string,
	config *types.DownloaderConfig) {

	key := config.Key()
	log.Printf("publishDownloaderConfig(%s/%s)\n", objType, config.Key())

	pub := downloaderPublication(ctx, objType)
	pub.Publish(key, config)
}

func unpublishDownloaderConfig(ctx *zedagentContext, objType string,
	config *types.DownloaderConfig) {

	key := config.Key()
	log.Printf("removeDownloaderConfig(%s/%s)\n", objType, key)

	pub := downloaderPublication(ctx, objType)
	c, _ := pub.Get(key)
	if c == nil {
		log.Printf("unpublishDownloaderConfig(%s) not found\n", key)
		return
	}
	pub.Unpublish(key)
}

func downloaderPublication(ctx *zedagentContext, objType string) *pubsub.Publication {
	var pub *pubsub.Publication
	switch objType {
	case baseOsObj:
		pub = ctx.pubBaseOsDownloadConfig
	case certObj:
		pub = ctx.pubCertObjDownloadConfig
	default:
		log.Fatalf("downloaderPublication: Unknown ObjType %s\n",
			objType)
	}
	return pub
}

func downloaderSubscription(ctx *zedagentContext, objType string) *pubsub.Subscription {
	var sub *pubsub.Subscription
	switch objType {
	case baseOsObj:
		sub = ctx.subBaseOsDownloadStatus
	case certObj:
		sub = ctx.subCertObjDownloadStatus
	default:
		log.Fatalf("downloaderSubscription: Unknown ObjType %s\n",
			objType)
	}
	return sub
}

func downloaderGetAll(ctx *zedagentContext) map[string]interface{} {
	sub1 := downloaderSubscription(ctx, baseOsObj)
	items1 := sub1.GetAll()
	sub2 := downloaderSubscription(ctx, certObj)
	items2 := sub2.GetAll()

	items := make(map[string]interface{})
	for k, i := range items1 {
		items[k] = i
	}
	for k, i := range items2 {
		items[k] = i
	}
	return items
}
