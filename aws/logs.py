from typing import Dict, Any
import sys
import time
import datetime
import aws
from aws import stderr


def most_recent_stream(group_name):
    stream = aws.client('logs').describe_log_streams(logGroupName=group_name, orderBy='LastEventTime', descending=True)['logStreams'][0]
    return stream['logStreamName']

def tail(group_name, follow=False, timestamps=False, exit_after=None):
    stderr('group:', group_name)
    if follow:
        token = ''
        last_stream = None
        limit = 3 # when starting to follow, dont page all history, just grab the last few entries and then start following
        while True:
            try:
                stream_name = most_recent_stream(group_name)
                if last_stream != stream_name:
                    last_stream = stream_name
                    token = ''
                    stderr('group:', group_name, 'stream:', stream_name)
            except (IndexError, aws.client('logs').exceptions.ResourceNotFoundException):
                pass
            else:
                kw: Dict[str, Any] = {}
                if token:
                    kw['nextToken'] = token
                if limit != 0:
                    kw['limit'] = limit
                    limit = 0
                resp = aws.client('logs').get_log_events(logGroupName=group_name, logStreamName=stream_name, **kw)
                if resp['events']:
                    token = resp['nextForwardToken']
                for log in resp['events']:
                    if log['message'].split()[0] not in ['START', 'END', 'REPORT']:
                        if timestamps:
                            print(datetime.datetime.fromtimestamp(log['timestamp'] / 1000), log['message'].replace('\t', ' ').strip(), flush=True)
                        else:
                            print(log['message'].replace('\t', ' ').strip(), flush=True)
                    if exit_after and exit_after in log['message']:
                        sys.exit(0)
            time.sleep(1)
    else:
        try:
            stream_name = most_recent_stream(group_name)
        except IndexError:
            stderr('no logs available')
            sys.exit(1)
        else:
            stderr('group:', group_name, 'stream:', stream_name)
            logs = aws.client('logs').get_log_events(logGroupName=group_name, logStreamName=stream_name)['events']
            for log in logs:
                if log['message'].split()[0] not in ['START', 'END', 'REPORT']:
                    if timestamps:
                        print(datetime.datetime.fromtimestamp(log['timestamp'] / 1000), log['message'].replace('\t', ' ').strip(), flush=True)
                    else:
                        print(log['message'].replace('\t', ' ').strip(), flush=True)
