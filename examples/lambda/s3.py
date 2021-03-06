#!/usr/bin/env python3
#
# conf: concurrency 0
# conf: memory 128
# conf: timeout 60
# policy: AWSLambdaBasicExecutionRole
# s3: ${bucket}
# trigger: s3 ${bucket}

import boto3

s3 = boto3.client('s3')

def main(event, context):
    """
    >>> import shell, uuid
    >>> run = lambda *a, **kw: shell.run(*a, stream=True, **kw)
    >>> path = __file__
    >>> bucket = f'cli-aws-{str(uuid.uuid4())[-12:]}'
    >>> uid = str(uuid.uuid4())[-12:]

    >>> _ = run(f'aws-lambda-rm -ey {path}')

    >>> _ = run(f'bucket={bucket} aws-lambda-deploy -y {path} && sleep 5 # iam is slow')

    >>> _ = run(f'echo | aws s3 cp - s3://{bucket}/{uid}')

    >>> assert uid == run(f'aws-lambda-logs {path} -f -e {uid} | tail -n1').split()[-1]

    >>> _ = run(f'bucket={bucket} aws-lambda-rm -ey', path)

    """
    for record in event['Records']:
        print(record['s3']['object']['key'])
