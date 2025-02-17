package quark

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"net/http"
	"os"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/errs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

type Quark struct {
	model.Storage
	Addition
}

func (d *Quark) Config() driver.Config {
	return config
}

func (d *Quark) GetAddition() driver.Additional {
	return d.Addition
}

func (d *Quark) Init(ctx context.Context, storage model.Storage) error {
	d.Storage = storage
	err := utils.Json.UnmarshalFromString(d.Storage.Addition, &d.Addition)
	if err != nil {
		return err
	}
	_, err = d.request("/config", http.MethodGet, nil, nil)
	return err
}

func (d *Quark) Drop(ctx context.Context) error {
	return nil
}

func (d *Quark) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.GetFiles(dir.GetID())
	if err != nil {
		return nil, err
	}
	objs := make([]model.Obj, len(files))
	for i := 0; i < len(files); i++ {
		objs[i] = fileToObj(files[i])
	}
	return objs, nil
}

//func (d *Quark) Get(ctx context.Context, path string) (model.Obj, error) {
//	// TODO this is optional
//	return nil, errs.NotImplement
//}

func (d *Quark) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	data := base.Json{
		"fids": []string{file.GetID()},
	}
	var resp DownResp
	_, err := d.request("/file/download", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, &resp)
	if err != nil {
		return nil, err
	}
	return &model.Link{
		URL: resp.Data[0].DownloadUrl,
		Header: http.Header{
			"Cookie":  []string{d.Cookie},
			"Referer": []string{"https://pan.quark.cn"},
		},
	}, nil
}

func (d *Quark) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	data := base.Json{
		"dir_init_lock": false,
		"dir_path":      "",
		"file_name":     dirName,
		"pdir_fid":      parentDir.GetID(),
	}
	_, err := d.request("/file", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *Quark) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	data := base.Json{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     []string{srcObj.GetID()},
		"to_pdir_fid":  dstDir.GetID(),
	}
	_, err := d.request("/file/move", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *Quark) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	data := base.Json{
		"fid":       srcObj.GetID(),
		"file_name": newName,
	}
	_, err := d.request("/file/rename", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *Quark) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	return errs.NotSupport
}

func (d *Quark) Remove(ctx context.Context, obj model.Obj) error {
	data := base.Json{
		"action_type":  1,
		"exclude_fids": []string{},
		"filelist":     []string{obj.GetID()},
	}
	_, err := d.request("/file/delete", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	return err
}

func (d *Quark) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	var tempFile *os.File
	var err error
	if f, ok := stream.GetReadCloser().(*os.File); ok {
		tempFile = f
	} else {
		tempFile, err = os.CreateTemp(conf.Conf.TempDir, "file-*")
		if err != nil {
			return err
		}
		defer func() {
			_ = tempFile.Close()
			_ = os.Remove(tempFile.Name())
		}()
		_, err = io.Copy(tempFile, stream)
		if err != nil {
			return err
		}
		_, err = tempFile.Seek(0, io.SeekStart)
		if err != nil {
			return err
		}
	}
	m := md5.New()
	_, err = io.Copy(m, tempFile)
	if err != nil {
		return err
	}
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}
	md5Str := hex.EncodeToString(m.Sum(nil))
	s := sha1.New()
	_, err = io.Copy(s, tempFile)
	if err != nil {
		return err
	}
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}
	sha1Str := hex.EncodeToString(s.Sum(nil))
	// pre
	pre, err := d.upPre(stream, dstDir.GetID())
	if err != nil {
		return err
	}
	log.Debugln("hash: ", md5Str, sha1Str)
	// hash
	finish, err := d.upHash(md5Str, sha1Str, pre.Data.TaskId)
	if err != nil {
		return err
	}
	if finish {
		return nil
	}
	// part up
	partSize := pre.Metadata.PartSize
	var bytes []byte
	md5s := make([]string, 0)
	defaultBytes := make([]byte, partSize)
	left := stream.GetSize()
	partNumber := 1
	sizeDivide100 := stream.GetSize() / 100
	for left > 0 {
		if left > int64(partSize) {
			bytes = defaultBytes
		} else {
			bytes = make([]byte, left)
		}
		_, err := io.ReadFull(tempFile, bytes)
		if err != nil {
			return err
		}
		left -= int64(partSize)
		log.Debugf("left: %d", left)
		m, err := d.upPart(pre, stream.GetMimetype(), partNumber, bytes)
		//m, err := driver.UpPart(pre, file.GetMIMEType(), partNumber, bytes, account, md5Str, sha1Str)
		if err != nil {
			return err
		}
		if m == "finish" {
			return nil
		}
		md5s = append(md5s, m)
		partNumber++
		up(100 - int(left/sizeDivide100))
	}
	err = d.upCommit(pre, md5s)
	if err != nil {
		return err
	}
	return d.upFinish(pre)
}

func (d *Quark) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
	return nil, errs.NotSupport
}

var _ driver.Driver = (*Quark)(nil)
