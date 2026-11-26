"""Reading original files from object storage (RustFS locally, Amazon S3 in production).

The RAG service only reads. In production its credentials allow GetObject on the documents
bucket and nothing else.
"""

import boto3
from botocore.config import Config as BotoConfig
from botocore.exceptions import ClientError

from rag.domain.documents import FileMissingError


class ObjectStore:
    def __init__(self, endpoint: str, bucket: str, access_key: str, secret_key: str, region: str):
        self._bucket = bucket
        self._client = boto3.client(
            "s3",
            endpoint_url=endpoint,
            aws_access_key_id=access_key,
            aws_secret_access_key=secret_key,
            region_name=region,
            # RustFS, like MinIO, serves buckets as paths rather than subdomains.
            config=BotoConfig(s3={"addressing_style": "path"}, retries={"max_attempts": 2}),
        )

    def read(self, key: str) -> bytes:
        try:
            return self._client.get_object(Bucket=self._bucket, Key=key)["Body"].read()
        except ClientError as exc:
            if exc.response.get("Error", {}).get("Code") in ("NoSuchKey", "404"):
                raise FileMissingError(key) from exc
            raise
