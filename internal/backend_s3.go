// Copyright 2019 Ka-Hing Cheung
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

package internal

import (
	"fmt"
	"syscall"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
)

type S3Backend struct {
	*s3.S3
	fs *Goofys

	aws bool
}

func (s *S3Backend) ListObjectsV2(params *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	if s.aws {
		return s.S3.ListObjectsV2(params)
	} else {
		v1 := s3.ListObjectsInput{
			Bucket:       params.Bucket,
			Delimiter:    params.Delimiter,
			EncodingType: params.EncodingType,
			MaxKeys:      params.MaxKeys,
			Prefix:       params.Prefix,
			RequestPayer: params.RequestPayer,
		}
		if params.StartAfter != nil {
			v1.Marker = params.StartAfter
		} else {
			v1.Marker = params.ContinuationToken
		}

		objs, err := s.S3.ListObjects(&v1)
		if err != nil {
			return nil, err
		}

		count := int64(len(objs.Contents))
		v2Objs := s3.ListObjectsV2Output{
			CommonPrefixes:        objs.CommonPrefixes,
			Contents:              objs.Contents,
			ContinuationToken:     objs.Marker,
			Delimiter:             objs.Delimiter,
			EncodingType:          objs.EncodingType,
			IsTruncated:           objs.IsTruncated,
			KeyCount:              &count,
			MaxKeys:               objs.MaxKeys,
			Name:                  objs.Name,
			NextContinuationToken: objs.NextMarker,
			Prefix:                objs.Prefix,
			StartAfter:            objs.Marker,
		}

		return &v2Objs, nil
	}
}

func (s *S3Backend) HeadBlob(param *HeadBlobInput) (*HeadBlobOutput, error) {
	resp, err := s.S3.HeadObject(&s3.HeadObjectInput{
		Bucket: &s.fs.bucket,
		Key:    &param.Key,
	})
	if err != nil {
		return nil, mapAwsError(err)
	}
	return &HeadBlobOutput{
		BlobItemOutput: BlobItemOutput{
			Key:          &param.Key,
			ETag:         resp.ETag,
			LastModified: resp.LastModified,
			Size:         uint64(*resp.ContentLength),
			StorageClass: resp.StorageClass,
		},
		ContentType: resp.ContentType,
		Metadata:    resp.Metadata,
	}, nil
}

func (s *S3Backend) ListBlobs(param *ListBlobsInput) (*ListBlobsOutput, error) {
	var maxKeys *int64

	if param.MaxKeys != nil {
		maxKeys = aws.Int64(int64(*param.MaxKeys))
	}

	resp, err := s.ListObjectsV2(&s3.ListObjectsV2Input{
		Bucket:            &s.fs.bucket,
		Prefix:            param.Prefix,
		Delimiter:         param.Delimiter,
		MaxKeys:           maxKeys,
		StartAfter:        param.StartAfter,
		ContinuationToken: param.ContinuationToken,
	})
	if err != nil {
		return nil, mapAwsError(err)
	}

	prefixes := make([]BlobPrefixOutput, 0)
	items := make([]BlobItemOutput, 0)

	for _, p := range resp.CommonPrefixes {
		prefixes = append(prefixes, BlobPrefixOutput{Prefix: p.Prefix})
	}
	for _, i := range resp.Contents {
		items = append(items, BlobItemOutput{
			Key:          i.Key,
			ETag:         i.ETag,
			LastModified: i.LastModified,
			Size:         uint64(*i.Size),
			StorageClass: i.StorageClass,
		})
	}

	return &ListBlobsOutput{
		ContinuationToken:     param.ContinuationToken,
		Prefixes:              prefixes,
		Items:                 items,
		NextContinuationToken: resp.NextContinuationToken,
		IsTruncated:           *resp.IsTruncated,
	}, nil
}

func (s *S3Backend) RenameBlob(param *RenameBlobInput) (*RenameBlobOutput, error) {
	return nil, syscall.ENOTSUP
}

func (s *S3Backend) mpuCopyPart(from string, to string, mpuId string, bytes string, part int64,
	sem semaphore, srcEtag *string, etag **string, errout *error) {

	defer sem.P(1)

	// XXX use CopySourceIfUnmodifiedSince to ensure that
	// we are copying from the same object
	params := &s3.UploadPartCopyInput{
		Bucket:            &s.fs.bucket,
		Key:               &to,
		CopySource:        aws.String(pathEscape(from)),
		UploadId:          &mpuId,
		CopySourceRange:   &bytes,
		CopySourceIfMatch: srcEtag,
		PartNumber:        &part,
	}

	s3Log.Debug(params)

	resp, err := s.fs.s3.UploadPartCopy(params)
	if err != nil {
		s3Log.Errorf("UploadPartCopy %v = %v", params, err)
		*errout = mapAwsError(err)
		return
	}

	*etag = resp.CopyPartResult.ETag
	return
}

func sizeToParts(size int64) (int, int64) {
	const MAX_S3_MPU_SIZE = 5 * 1024 * 1024 * 1024 * 1024
	if size > MAX_S3_MPU_SIZE {
		panic(fmt.Sprintf("object size: %v exceeds maximum S3 MPU size: %v", size, MAX_S3_MPU_SIZE))
	}

	// Use the maximum number of parts to allow the most server-side copy
	// parallelism.
	const MAX_PARTS = 10 * 1000
	const MIN_PART_SIZE = 50 * 1024 * 1024
	partSize := MaxInt64(size/(MAX_PARTS-1), MIN_PART_SIZE)

	nParts := int(size / partSize)
	if size%partSize != 0 {
		nParts++
	}

	return nParts, partSize
}

func (s *S3Backend) mpuCopyParts(size int64, from string, to string, mpuId string,
	srcEtag *string, etags []*string, partSize int64, err *error) {

	rangeFrom := int64(0)
	rangeTo := int64(0)

	MAX_CONCURRENCY := MinInt(100, len(etags))
	sem := make(semaphore, MAX_CONCURRENCY)
	sem.P(MAX_CONCURRENCY)

	for i := int64(1); rangeTo < size; i++ {
		rangeFrom = rangeTo
		rangeTo = i * partSize
		if rangeTo > size {
			rangeTo = size
		}
		bytes := fmt.Sprintf("bytes=%v-%v", rangeFrom, rangeTo-1)

		sem.V(1)
		go s.mpuCopyPart(from, to, mpuId, bytes, i, sem, srcEtag, &etags[i-1], err)
	}
}

func (s *S3Backend) copyObjectMultipart(size int64, from string, to string, mpuId string,
	srcEtag *string, metadata map[string]*string) (err error) {
	nParts, partSize := sizeToParts(size)
	etags := make([]*string, nParts)

	if mpuId == "" {
		params := &s3.CreateMultipartUploadInput{
			Bucket:       &s.fs.bucket,
			Key:          &to,
			StorageClass: &s.fs.flags.StorageClass,
			ContentType:  s.fs.getMimeType(to),
			Metadata:     metadata,
		}

		if s.fs.flags.UseSSE {
			params.ServerSideEncryption = &s.fs.sseType
			if s.fs.flags.UseKMS && s.fs.flags.KMSKeyID != "" {
				params.SSEKMSKeyId = &s.fs.flags.KMSKeyID
			}
		}

		if s.fs.flags.ACL != "" {
			params.ACL = &s.fs.flags.ACL
		}

		resp, err := s.CreateMultipartUpload(params)
		if err != nil {
			return mapAwsError(err)
		}

		mpuId = *resp.UploadId
	}

	s.mpuCopyParts(size, from, to, mpuId, srcEtag, etags, partSize, &err)

	if err != nil {
		return
	} else {
		parts := make([]*s3.CompletedPart, nParts)
		for i := 0; i < nParts; i++ {
			parts[i] = &s3.CompletedPart{
				ETag:       etags[i],
				PartNumber: aws.Int64(int64(i + 1)),
			}
		}

		params := &s3.CompleteMultipartUploadInput{
			Bucket:   &s.fs.bucket,
			Key:      &to,
			UploadId: &mpuId,
			MultipartUpload: &s3.CompletedMultipartUpload{
				Parts: parts,
			},
		}

		s3Log.Debug(params)

		_, err := s.CompleteMultipartUpload(params)
		if err != nil {
			s3Log.Errorf("Complete MPU %v = %v", params, err)
			return mapAwsError(err)
		}
	}

	return
}

func (s *S3Backend) CopyBlob(param *CopyBlobInput) (*CopyBlobOutput, error) {
	if param.Size == nil || param.ETag == nil || param.Metadata == nil {
		params := &HeadBlobInput{Key: param.Source}
		resp, err := s.HeadBlob(params)
		if err != nil {
			return nil, err
		}

		param.Size = &resp.Size
		param.Metadata = resp.Metadata
		param.ETag = resp.ETag
	}

	from := s.fs.bucket + "/" + param.Source

	if !s.fs.gcs && *param.Size > 5*1024*1024*1024 {
		err := s.copyObjectMultipart(int64(*param.Size), from, param.Destination, "", param.ETag, param.Metadata)
		if err != nil {
			return nil, err
		}
		return &CopyBlobOutput{}, nil
	}

	storageClass := s.fs.flags.StorageClass
	if *param.Size < 128*1024 && storageClass == "STANDARD_IA" {
		storageClass = "STANDARD"
	}

	params := &s3.CopyObjectInput{
		Bucket:            &s.fs.bucket,
		CopySource:        aws.String(pathEscape(from)),
		Key:               &param.Destination,
		StorageClass:      &storageClass,
		ContentType:       s.fs.getMimeType(param.Destination),
		Metadata:          param.Metadata,
		MetadataDirective: aws.String(s3.MetadataDirectiveReplace),
	}

	s3Log.Debug(params)

	if s.fs.flags.UseSSE {
		params.ServerSideEncryption = &s.fs.sseType
		if s.fs.flags.UseKMS && s.fs.flags.KMSKeyID != "" {
			params.SSEKMSKeyId = &s.fs.flags.KMSKeyID
		}
	}

	if s.fs.flags.ACL != "" {
		params.ACL = &s.fs.flags.ACL
	}

	_, err := s.CopyObject(params)
	if err != nil {
		s3Log.Errorf("CopyObject %v = %v", params, err)
		return nil, mapAwsError(err)
	}

	return &CopyBlobOutput{}, nil
}

func (s *S3Backend) GetBlob(param *GetBlobInput) (*GetBlobOutput, error) {
	get := s3.GetObjectInput{
		Bucket: &s.fs.bucket,
		Key:    &param.Key,
	}

	if param.Start != 0 || param.Count != 0 {
		var bytes string
		if param.Count != 0 {
			bytes = fmt.Sprintf("bytes=%v-%v", param.Start, param.Start+param.Count-1)
		} else {
			bytes = fmt.Sprintf("bytes=%v-", param.Start)
		}
		get.Range = &bytes
	}
	// TODO handle IfMatch

	resp, err := s.GetObject(&get)
	if err != nil {
		return nil, mapAwsError(err)
	}

	return &GetBlobOutput{
		HeadBlobOutput: HeadBlobOutput{
			BlobItemOutput: BlobItemOutput{
				Key:          &param.Key,
				ETag:         resp.ETag,
				LastModified: resp.LastModified,
				Size:         uint64(*resp.ContentLength),
				StorageClass: resp.StorageClass,
			},
			ContentType: resp.ContentType,
			Metadata:    resp.Metadata,
		},
		Body: resp.Body,
	}, nil
}

func (s *S3Backend) PutBlob(param *PutBlobInput) (*PutBlobOutput, error) {
	put := &s3.PutObjectInput{
		Bucket:       &s.fs.bucket,
		Key:          &param.Key,
		Metadata:     param.Metadata,
		Body:         param.Body,
		StorageClass: param.StorageClass,
		ContentType:  param.ContentType,
	}

	if s.fs.flags.UseSSE {
		put.ServerSideEncryption = &s.fs.sseType
		if s.fs.flags.UseKMS && s.fs.flags.KMSKeyID != "" {
			put.SSEKMSKeyId = &s.fs.flags.KMSKeyID
		}
	}

	if s.fs.flags.ACL != "" {
		put.ACL = &s.fs.flags.ACL
	}

	resp, err := s.PutObject(put)
	if err != nil {
		return nil, mapAwsError(err)
	}

	return &PutBlobOutput{
		ETag: resp.ETag,
	}, nil
}

// MultipartBlobBegin(param *MultipartBlobBeginInput) (*MultipartBlobCommitInput, error)
// MultipartBlobAdd(param *MultipartBlobAddInput) (*MultipartBlobAddOutput, error)
// MultipartBlobCommit(param *MultipartBlobCommitInput) (*MultipartBlobCommitOutput, error)
// MultipartExpire(param *MultipartExpireInput) (*MultipartExpireOutput, error)