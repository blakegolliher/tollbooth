# Copyright 2026 Blake Golliher
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""boto3 leg of the throttling matrix (test plan category 7).

Uploads OBJECTS objects of SIZE bytes with CONCURRENCY threads through tollbooth using boto3's own
retry settings (whatever AWS_RETRY_MODE / AWS_MAX_ATTEMPTS say; unset means botocore's defaults),
then prints one line `objects N ok K failed F seconds S` for throttle.sh. Environment: ENDPOINT,
REGION, BUCKET, OBJECTS, CONCURRENCY, SIZE, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY.
"""

import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor

import boto3
from botocore.config import Config

ENDPOINT = os.environ["ENDPOINT"]
BUCKET = os.environ["BUCKET"]
OBJECTS = int(os.environ.get("OBJECTS", "200"))
CONCURRENCY = int(os.environ.get("CONCURRENCY", "20"))
SIZE = int(os.environ.get("SIZE", "65536"))

s3 = boto3.client(
    "s3",
    endpoint_url=ENDPOINT,
    region_name=os.environ.get("REGION", "us-east-1"),
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}, max_pool_connections=CONCURRENCY),
)
body = os.urandom(SIZE)


def put(i):
    try:
        s3.put_object(Bucket=BUCKET, Key=f"sync/{i:05d}.bin", Body=body)
        return None
    except Exception as e:  # noqa: BLE001 - every failure is a result
        return f"{type(e).__name__}: {str(e)[:120]}"


start = time.monotonic()
with ThreadPoolExecutor(max_workers=CONCURRENCY) as pool:
    results = list(pool.map(put, range(OBJECTS)))
elapsed = time.monotonic() - start
failed = [r for r in results if r]
for r in failed[:3]:
    print("failure:", r, file=sys.stderr)
print(f"objects {OBJECTS} ok {OBJECTS - len(failed)} failed {len(failed)} seconds {elapsed:.1f}")
sys.exit(1 if failed else 0)
