/*
Copyright 2011 The Perkeep Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package s3

import (
	"context"

	"perkeep.org/pkg/blob"

	"go4.org/syncutil"
)

var removeGate = syncutil.NewGate(20) // arbitrary

func (sto *s3Storage) RemoveBlobs(ctx context.Context, blobs []blob.Ref) error {
	var wg syncutil.Group

	for _, blob := range blobs {
		blob := blob
		removeGate.Start()
		wg.Go(func() error {
			defer removeGate.Done()
			return sto.s3Client.Delete(ctx, sto.bucket, sto.dirPrefix+blob.String())
		})
	}
	return wg.Err()

}
