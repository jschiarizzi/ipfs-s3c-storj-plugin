package s3

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"path"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	ds "gx/ipfs/QmUadX5EcvrBmxAV9sE7wUWtWSqxns5K84qKJBixmcT1w9/go-datastore"
	dsq "gx/ipfs/QmUadX5EcvrBmxAV9sE7wUWtWSqxns5K84qKJBixmcT1w9/go-datastore/query"
)

const (
	// listMax is the largest amount of objects you can request from S3 in a list
	// call.QmUadX5EcvrBmxAV9sE7wUWtWSqxns5K84qKJBixmcT1w9
	listMax = 1000

	// deleteMax is the largest amount of objects you can delete from S3 in a
	// delete objects call.
	deleteMax = 1000

	defaultWorkers = 100
)

type S3Bucket struct {
	Config
	S3 *s3.S3
}

type Config struct {
	AccessKey string
	SecretKey string
	//	SessionToken   string
	Bucket        string
	Region        string
	Endpoint      string
	RootDirectory string
	Workers       int
}

func NewS3Datastore(conf Config) (*S3Bucket, error) {
	if conf.Workers == 0 {
		conf.Workers = defaultWorkers
	}

	// Configure to use Minio Server
	s3Config := &aws.Config{
		// TODO: determine if we need session token
		Credentials: credentials.NewStaticCredentials(conf.AccessKey, conf.SecretKey, ""),
		Endpoint:    aws.String(conf.Endpoint),
		Region:      aws.String(conf.Region),
		//		DisableSSL:       aws.Bool(conf.Secure),
		S3ForcePathStyle: aws.Bool(true),
	}
	s3Session, err := session.NewSession(s3Config)
	if err != nil {
		return nil, err
	}

	return &S3Bucket{
		S3:     s3.New(s3Session),
		Config: conf,
	}, nil
}

func (s *S3Bucket) Put(k ds.Key, value []byte) error {
	_, err := s.S3.PutObject(&s3.PutObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.s3Path(k.String())),
		Body:   bytes.NewReader(value),
	})
	return parseError(err)
}

func (s *S3Bucket) Get(k ds.Key) ([]byte, error) {
	resp, err := s.S3.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.s3Path(k.String())),
	})
	if err != nil {
		return nil, parseError(err)
	}
	defer resp.Body.Close()

	return ioutil.ReadAll(resp.Body)
}

func (s *S3Bucket) Has(k ds.Key) (exists bool, err error) {
	_, err = s.GetSize(k)
	if err != nil {
		if err == ds.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *S3Bucket) GetSize(k ds.Key) (size int, err error) {
	resp, err := s.S3.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.s3Path(k.String())),
	})
	if err != nil {
		if s3Err, ok := err.(awserr.Error); ok && s3Err.Code() == "NotFound" {
			return -1, ds.ErrNotFound
		}
		return -1, err
	}
	return int(*resp.ContentLength), nil
}

func (s *S3Bucket) Delete(k ds.Key) error {
	_, err := s.S3.DeleteObject(&s3.DeleteObjectInput{
		Bucket: aws.String(s.Bucket),
		Key:    aws.String(s.s3Path(k.String())),
	})
	return parseError(err)
}

func (s *S3Bucket) Query(q dsq.Query) (dsq.Results, error) {
	if q.Orders != nil || q.Filters != nil {
		return nil, fmt.Errorf("s3ds: filters or orders are not supported")
	}

	limit := q.Limit + q.Offset
	if limit == 0 || limit > listMax {
		limit = listMax
	}

	resp, err := s.S3.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket:  aws.String(s.Bucket),
		Prefix:  aws.String(s.s3Path(q.Prefix)),
		MaxKeys: aws.Int64(int64(limit)),
	})
	if err != nil {
		return nil, err
	}

	index := q.Offset
	nextValue := func() (dsq.Result, bool) {
		for index >= len(resp.Contents) {
			if !*resp.IsTruncated {
				return dsq.Result{}, false
			}

			index -= len(resp.Contents)

			resp, err = s.S3.ListObjectsV2(&s3.ListObjectsV2Input{
				Bucket:            aws.String(s.Bucket),
				Prefix:            aws.String(s.s3Path(q.Prefix)),
				Delimiter:         aws.String("/"),
				MaxKeys:           aws.Int64(listMax),
				ContinuationToken: resp.NextContinuationToken,
			})
			if err != nil {
				return dsq.Result{Error: err}, false
			}
		}

		entry := dsq.Entry{
			Key: ds.NewKey(*resp.Contents[index].Key).String(),
		}
		if !q.KeysOnly {
			value, err := s.Get(ds.NewKey(entry.Key))
			if err != nil {
				return dsq.Result{Error: err}, false
			}
			entry.Value = value
		}

		index++
		return dsq.Result{Entry: entry}, true
	}

	return dsq.ResultsFromIterator(q, dsq.Iterator{
		Close: func() error {
			return nil
		},
		Next: nextValue,
	}), nil
}

func (s *S3Bucket) Batch() (ds.Batch, error) {
	return &s3Batch{
		s:          s,
		ops:        make(map[string]batchOp),
		numWorkers: s.Workers,
	}, nil
}

func (s *S3Bucket) Close() error {
	return nil
}

func (s *S3Bucket) s3Path(p string) string {
	return path.Join(s.RootDirectory, p)
}

func parseError(err error) error {
	if s3Err, ok := err.(awserr.Error); ok && s3Err.Code() == s3.ErrCodeNoSuchKey {
		return ds.ErrNotFound
	}
	return nil
}

type s3Batch struct {
	s          *S3Bucket
	ops        map[string]batchOp
	numWorkers int
}

type batchOp struct {
	val    []byte
	delete bool
}

func (b *s3Batch) Put(k ds.Key, val []byte) error {
	b.ops[k.String()] = batchOp{
		val:    val,
		delete: false,
	}
	return nil
}

func (b *s3Batch) Delete(k ds.Key) error {
	b.ops[k.String()] = batchOp{
		val:    nil,
		delete: true,
	}
	return nil
}

func (b *s3Batch) Commit() error {
	var (
		deleteObjs []*s3.ObjectIdentifier
		putKeys    []ds.Key
	)
	for k, op := range b.ops {
		if op.delete {
			deleteObjs = append(deleteObjs, &s3.ObjectIdentifier{
				Key: aws.String(k),
			})
		} else {
			putKeys = append(putKeys, ds.NewKey(k))
		}
	}

	numJobs := len(putKeys) + (len(deleteObjs) / deleteMax)
	jobs := make(chan func() error, numJobs)
	results := make(chan error, numJobs)

	numWorkers := b.numWorkers
	if numJobs < numWorkers {
		numWorkers = numJobs
	}

	var wg sync.WaitGroup
	wg.Add(numWorkers)
	defer wg.Wait()

	for w := 0; w < numWorkers; w++ {
		go func() {
			defer wg.Done()
			worker(jobs, results)
		}()
	}

	for _, k := range putKeys {
		jobs <- b.newPutJob(k, b.ops[k.String()].val)
	}

	if len(deleteObjs) > 0 {
		for i := 0; i < len(deleteObjs); i += deleteMax {
			limit := deleteMax
			if len(deleteObjs[i:]) < limit {
				limit = len(deleteObjs[i:])
			}

			jobs <- b.newDeleteJob(deleteObjs[i : i+limit])
		}
	}
	close(jobs)

	var errs []string
	for i := 0; i < numJobs; i++ {
		err := <-results
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("s3ds: failed batch operation:\n%s", strings.Join(errs, "\n"))
	}

	return nil
}

func (b *s3Batch) newPutJob(k ds.Key, value []byte) func() error {
	return func() error {
		return b.s.Put(k, value)
	}
}

func (b *s3Batch) newDeleteJob(objs []*s3.ObjectIdentifier) func() error {
	return func() error {
		resp, err := b.s.S3.DeleteObjects(&s3.DeleteObjectsInput{
			Bucket: aws.String(b.s.Bucket),
			Delete: &s3.Delete{
				Objects: objs,
			},
		})
		if err != nil {
			return err
		}

		var errs []string
		for _, err := range resp.Errors {
			errs = append(errs, err.String())
		}

		if len(errs) > 0 {
			return fmt.Errorf("failed to delete objects: %s", errs)
		}

		return nil
	}
}

func worker(jobs <-chan func() error, results chan<- error) {
	for j := range jobs {
		results <- j()
	}
}

var _ ds.Batching = (*S3Bucket)(nil)
