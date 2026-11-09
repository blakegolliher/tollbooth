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

"""boto3 leg of the client compatibility matrix (test plan category 6).

Runs one operation per line of the matrix through tollbooth and prints `op PASS|FAIL detail`,
one per line, so clients.sh can fold the results into docs/COMPAT.md. Environment:
ENDPOINT (http://host:port of tollbooth), REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY,
BUCKET (created and deleted here), ADDRESSING (path|virtual).
"""

import hashlib
import os
import sys
import urllib.error
import urllib.request

import boto3
from botocore.config import Config

ENDPOINT = os.environ["ENDPOINT"]
REGION = os.environ.get("REGION", "us-east-1")
BUCKET = os.environ["BUCKET"]
ADDRESSING = os.environ.get("ADDRESSING", "path")

s3 = boto3.client(
    "s3",
    endpoint_url=ENDPOINT,
    region_name=REGION,
    config=Config(
        signature_version="s3v4",  # botocore may presign with the legacy S3 signature for a custom endpoint otherwise
        s3={"addressing_style": ADDRESSING},
        retries={"max_attempts": 1},
        request_checksum_calculation="when_required",
        response_checksum_validation="when_required",
    ),
)

results = []


def case(name, fn):
    try:
        fn()
        results.append((name, "PASS", ""))
    except Exception as e:  # noqa: BLE001 - every failure is a matrix cell
        results.append((name, "FAIL", str(e).replace("\n", " ")[:200]))


def payload(n, seed=0x9E3779B9):
    out = bytearray(n)
    x = seed & 0xFFFFFFFF
    for i in range(n):
        x ^= (x << 13) & 0xFFFFFFFF
        x ^= x >> 17
        x ^= (x << 5) & 0xFFFFFFFF
        out[i] = x & 0xFF
    return bytes(out)


SMALL = payload(4096)
PART = payload(5 * 1024 * 1024)


def mb():
    s3.create_bucket(Bucket=BUCKET)
    s3.head_bucket(Bucket=BUCKET)


def cp_up():
    s3.put_object(Bucket=BUCKET, Key="dir/small.bin", Body=SMALL)
    s3.put_object(Bucket=BUCKET, Key="dir with space/ünï+%.bin", Body=b"x")


def cp_down():
    got = s3.get_object(Bucket=BUCKET, Key="dir/small.bin")["Body"].read()
    assert got == SMALL, "body mismatch"
    got = s3.get_object(Bucket=BUCKET, Key="dir/small.bin", Range="bytes=1-100")["Body"].read()
    assert got == SMALL[1:101], "range mismatch"


def ls():
    keys = {o["Key"] for o in s3.list_objects_v2(Bucket=BUCKET, Prefix="dir").get("Contents", [])}
    assert "dir/small.bin" in keys, keys
    v1 = s3.list_objects(Bucket=BUCKET, Prefix="dir/")
    assert any(o["Key"] == "dir/small.bin" for o in v1.get("Contents", [])), v1
    buckets = {b["Name"] for b in s3.list_buckets()["Buckets"]}
    assert BUCKET in buckets, buckets


def sync():
    # The equivalent of `aws s3 sync`: HEAD, conditional re-upload, list, as the SDK does it.
    for i in range(5):
        s3.put_object(Bucket=BUCKET, Key=f"sync/{i}.bin", Body=payload(1024, seed=i + 1))
    for i in range(5):
        h = s3.head_object(Bucket=BUCKET, Key=f"sync/{i}.bin")
        assert h["ContentLength"] == 1024, h
    assert len(s3.list_objects_v2(Bucket=BUCKET, Prefix="sync/")["Contents"]) == 5


def rm():
    s3.delete_object(Bucket=BUCKET, Key="dir with space/ünï+%.bin")
    r = s3.delete_objects(Bucket=BUCKET, Delete={"Objects": [{"Key": f"sync/{i}.bin"} for i in range(5)], "Quiet": False})
    assert len(r.get("Deleted", [])) == 5, r


def fetch(req):
    """urlopen that reports the backend's error body instead of a bare status."""
    try:
        with urllib.request.urlopen(req) as resp:  # noqa: S310 - test tool, local endpoint
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        raise AssertionError(f"HTTP {e.code}: {e.read()[:300]!r}") from None


def presign():
    url = s3.generate_presigned_url("get_object", Params={"Bucket": BUCKET, "Key": "dir/small.bin"}, ExpiresIn=300)
    status, body = fetch(url)
    assert body == SMALL, f"presigned body mismatch ({status})"
    url = s3.generate_presigned_url("put_object", Params={"Bucket": BUCKET, "Key": "dir/presigned-put.bin"}, ExpiresIn=300)
    status, _ = fetch(urllib.request.Request(url, data=SMALL, method="PUT"))
    assert status == 200, status
    assert s3.get_object(Bucket=BUCKET, Key="dir/presigned-put.bin")["Body"].read() == SMALL


def multipart():
    key = "dir/multipart.bin"
    up = s3.create_multipart_upload(Bucket=BUCKET, Key=key)
    uid = up["UploadId"]
    parts = []
    for n in range(1, 7):  # six parts: five of 5 MiB and a final 1 MiB
        body = PART if n < 6 else payload(1024 * 1024, seed=n)
        r = s3.upload_part(Bucket=BUCKET, Key=key, UploadId=uid, PartNumber=n, Body=body)
        parts.append({"ETag": r["ETag"], "PartNumber": n})
    listed = s3.list_parts(Bucket=BUCKET, Key=key, UploadId=uid)
    assert len(listed["Parts"]) == 6, listed
    s3.complete_multipart_upload(Bucket=BUCKET, Key=key, UploadId=uid, MultipartUpload={"Parts": parts})
    h = s3.head_object(Bucket=BUCKET, Key=key)
    assert h["ContentLength"] == 5 * 5 * 1024 * 1024 + 1024 * 1024, h
    # An aborted upload.
    up2 = s3.create_multipart_upload(Bucket=BUCKET, Key="dir/aborted.bin")
    s3.abort_multipart_upload(Bucket=BUCKET, Key="dir/aborted.bin", UploadId=up2["UploadId"])


def part_number():
    g = s3.get_object(Bucket=BUCKET, Key="dir/multipart.bin", PartNumber=2)
    assert g["Body"].read() == PART, "part 2 body mismatch"
    h = s3.head_object(Bucket=BUCKET, Key="dir/multipart.bin", PartNumber=2)
    assert h["ContentLength"] == len(PART), h
    assert h.get("PartsCount", 6) == 6, h


def attributes():
    a = s3.get_object_attributes(Bucket=BUCKET, Key="dir/small.bin", ObjectAttributes=["ETag", "ObjectSize"])
    assert a["ObjectSize"] == len(SMALL), a


def checksums():
    algos = ["CRC32", "CRC32C", "SHA1", "SHA256", "CRC64NVME"]
    for algo in algos:
        key = f"dir/checksum-{algo.lower()}.bin"
        s3.put_object(Bucket=BUCKET, Key=key, Body=SMALL, ChecksumAlgorithm=algo)
        h = s3.head_object(Bucket=BUCKET, Key=key, ChecksumMode="ENABLED")
        field = "Checksum" + algo.replace("NVME", "NVME")
        assert any(k.startswith("Checksum") for k in h), f"{algo}: no checksum echoed in {list(h)}"
        got = s3.get_object(Bucket=BUCKET, Key=key, ChecksumMode="ENABLED")["Body"].read()
        assert got == SMALL, f"{algo}: body mismatch"
    # A wrong declared checksum must be refused by the backend, through the proxy unchanged.
    try:
        s3.put_object(Bucket=BUCKET, Key="dir/badsum.bin", Body=SMALL, ChecksumSHA256="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
        raise AssertionError("wrong SHA256 checksum was accepted")
    except Exception as e:  # noqa: BLE001
        if "accepted" in str(e):
            raise
        if not any(code in str(e) for code in ("BadDigest", "InvalidDigest", "InvalidRequest", "XAmzContentSHA256Mismatch", "400")):
            raise AssertionError(f"unexpected refusal: {e}")


def rb():
    keys = [o["Key"] for o in s3.list_objects_v2(Bucket=BUCKET).get("Contents", [])]
    if keys:
        s3.delete_objects(Bucket=BUCKET, Delete={"Objects": [{"Key": k} for k in keys], "Quiet": True})
    s3.delete_bucket(Bucket=BUCKET)


for name, fn in [("mb", mb), ("cp", cp_up), ("get", cp_down), ("ls", ls), ("sync", sync), ("rm", rm),
                 ("presign", presign), ("multipart", multipart), ("part-number", part_number),
                 ("attributes", attributes), ("checksums", checksums), ("rb", rb)]:
    case(name, fn)

bad = 0
for name, status, detail in results:
    print(f"{name} {status} {detail}")
    bad += status == "FAIL"
sys.exit(1 if bad else 0)
