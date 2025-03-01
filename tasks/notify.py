import io
import json
import os
import traceback
from collections import defaultdict
from datetime import datetime
from typing import Dict

from invoke import task
from invoke.exceptions import Exit, UnexpectedExit

from tasks.libs.datadog_api import create_count, send_metrics
from tasks.libs.pipeline_data import get_failed_jobs
from tasks.libs.pipeline_notifications import (
    GITHUB_SLACK_MAP,
    base_message,
    check_for_missing_owners_slack_and_jira,
    find_job_owners,
    get_failed_tests,
    send_slack_message,
)
from tasks.libs.pipeline_stats import get_failed_jobs_stats
from tasks.libs.types import FailedJobs, SlackMessage, TeamMessage

UNKNOWN_OWNER_TEMPLATE = """The owner `{owner}` is not mapped to any slack channel.
Please check for typos in the JOBOWNERS file and/or add them to the Github <-> Slack map.
"""
PROJECT_NAME = "DataDog/datadog-agent"
AWS_S3_CP_CMD = "aws s3 cp --only-show-errors --region us-east-1 --sse AES256"
S3_CI_BUCKET_URL = "s3://dd-ci-artefacts-build-stable/datadog-agent/failed_jobs"
JOB_FAILURES_FILE = "job_executions.json"
CONSECUTIVE_THRESHOLD = 3
CUMULATIVE_THRESHOLD = 5
CUMULATIVE_LENGTH = 10


@task
def check_teams(_):
    if check_for_missing_owners_slack_and_jira():
        print(
            "Error: Some teams in CODEOWNERS don't have their slack notification channel or jira specified!\n"
            "Please specify one in the GITHUB_SLACK_MAP or GITHUB_JIRA_MAP map in tasks/libs/pipeline_notifications.py."
        )
        raise Exit(code=1)
    else:
        print("All CODEOWNERS teams have their slack notification channel and jira project specified !!")


@task
def send_message(_, notification_type="merge", print_to_stdout=False):
    """
    Send notifications for the current pipeline. CI-only task.
    Use the --print-to-stdout option to test this locally, without sending
    real slack messages.
    """
    try:
        failed_jobs = get_failed_jobs(PROJECT_NAME, os.getenv("CI_PIPELINE_ID"))
        messages_to_send = generate_failure_messages(PROJECT_NAME, failed_jobs)
    except Exception as e:
        buffer = io.StringIO()
        print(base_message("datadog-agent", "is in an unknown state"), file=buffer)
        print("Found exception when generating notification:", file=buffer)
        traceback.print_exc(limit=-1, file=buffer)
        print("See the notify job log for the full exception traceback.", file=buffer)

        messages_to_send = {
            "@DataDog/agent-all": SlackMessage(base=buffer.getvalue()),
        }
        # Print traceback on job log
        print(e)
        traceback.print_exc()
        raise Exit(code=1)

    # From the job failures, set whether the pipeline succeeded or failed and craft the
    # base message that will be sent.
    if failed_jobs.all_mandatory_failures():  # At least one mandatory job failed
        header_icon = ":host-red:"
        state = "failed"
        coda = "If there is something wrong with the notification please contact #agent-platform"
    else:
        header_icon = ":host-green:"
        state = "succeeded"
        coda = ""

    header = ""
    if notification_type == "merge":
        header = f"{header_icon} :merged: datadog-agent merge"
    elif notification_type == "deploy":
        header = f"{header_icon} :rocket: datadog-agent deploy"
    base = base_message(header, state)

    # Send messages
    for owner, message in messages_to_send.items():
        channel = GITHUB_SLACK_MAP.get(owner.lower(), None)
        message.base_message = base
        if channel is None:
            channel = "#datadog-agent-pipelines"
            message.base_message += UNKNOWN_OWNER_TEMPLATE.format(owner=owner)
        message.coda = coda
        if print_to_stdout:
            print(f"Would send to {channel}:\n{str(message)}")
        else:
            send_slack_message(channel, str(message))  # TODO: use channel variable


@task
def send_stats(_, print_to_stdout=False):
    """
    Send statistics to Datadog for the current pipeline. CI-only task.
    Use the --print-to-stdout option to test this locally, without sending
    data points to Datadog.
    """
    try:
        global_failure_reason, job_failure_stats = get_failed_jobs_stats(PROJECT_NAME, os.getenv("CI_PIPELINE_ID"))
    except Exception as e:
        print("Found exception when generating statistics:")
        print(e)
        traceback.print_exc(limit=-1)
        raise Exit(code=1)

    if not (print_to_stdout or os.environ.get("DD_API_KEY")):
        print("DD_API_KEY environment variable not set, cannot send pipeline metrics to the backend")
        raise Exit(code=1)

    timestamp = int(datetime.now().timestamp())
    series = []

    for failure_tags, count in job_failure_stats.items():
        # This allows getting stats on the number of jobs that fail due to infrastructure
        # issues vs. other failures, and have a per-pipeline ratio of infrastructure failures.
        series.append(
            create_count(
                metric_name="datadog.ci.job_failures",
                timestamp=timestamp,
                value=count,
                tags=list(failure_tags)
                + [
                    "repository:datadog-agent",
                    f"git_ref:{os.getenv('CI_COMMIT_REF_NAME')}",
                ],
            )
        )

    if job_failure_stats:  # At least one job failed
        pipeline_state = "failed"
    else:
        pipeline_state = "succeeded"

    pipeline_tags = [
        "repository:datadog-agent",
        f"git_ref:{os.getenv('CI_COMMIT_REF_NAME')}",
        f"status:{pipeline_state}",
    ]
    if global_failure_reason:  # Only set the reason if the pipeline fails
        pipeline_tags.append(f"reason:{global_failure_reason}")

    series.append(
        create_count(
            metric_name="datadog.ci.pipelines",
            timestamp=timestamp,
            value=1,
            tags=pipeline_tags,
        )
    )

    if not print_to_stdout:
        response = send_metrics(series)
        if response["errors"]:
            print(f"Error(s) while sending pipeline metrics to the Datadog backend: {response['errors']}")
            raise Exit(code=1)
        print(f"Sent pipeline metrics: {series}")
    else:
        print(f"Would send: {series}")


# Tasks to trigger pipeline notifications


def generate_failure_messages(project_name: str, failed_jobs: FailedJobs) -> Dict[str, SlackMessage]:
    all_teams = "@DataDog/agent-all"

    # Generate messages for each team
    messages_to_send = defaultdict(TeamMessage)
    messages_to_send[all_teams] = SlackMessage(jobs=failed_jobs)

    failed_job_owners = find_job_owners(failed_jobs)
    for owner, jobs in failed_job_owners.items():
        if owner == "@DataDog/multiple":
            for job in jobs.all_non_infra_failures():
                for test in get_failed_tests(project_name, job):
                    messages_to_send[all_teams].add_test_failure(test, job)
                    for owner in test.owners:
                        messages_to_send[owner].add_test_failure(test, job)
        elif owner == "@DataDog/do-not-notify":
            # Jobs owned by @DataDog/do-not-notify do not send team messages
            pass
        elif owner == all_teams:
            # Jobs owned by @DataDog/agent-all will already be in the global
            # message, do not overwrite the failed jobs list
            pass
        else:
            messages_to_send[owner].failed_jobs = jobs

    return messages_to_send


@task
def check_consistent_failures(ctx):
    # Retrieve the stored document in aws s3. It has the following format:
    # {
    #     "pipeline_id": 123,
    #     "jobs": {
    #         "job1": {"consecutive_failures": 2, "cumulative_failures": [0, 0, 0, 1, 0, 1, 1, 0, 1, 1]},
    #         "job2": {"consecutive_failures": 0, "cumulative_failures": [1, 0, 0, 0, 0, 0, 0, 0, 0, 0]},
    #         "job1": {"consecutive_failures": 1, "cumulative_failures": [1]},
    #     }
    # }
    # The pipeline_id is used to by-pass the check if the pipeline chronological order is not respected
    # The jobs dictionary contains the consecutive and cumulative failures for each job
    # The consecutive failures are reset to 0 when the job is not failing, and are raising an alert when reaching the CONSECUTIVE_THRESHOLD (3)
    # The cumulative failures list contains 1 for failures, 0 for succes. They contain only then CUMULATIVE_LENGTH(10) last executions and raise alert when 50% failure rate is reached
    job_executions = retrieve_job_executions(ctx)

    # By-pass if the pipeline chronological order is not respected
    if job_executions.get("pipeline_id", 0) > int(os.getenv("CI_PIPELINE_ID")):
        return
    job_executions["pipeline_id"] = int(os.getenv("CI_PIPELINE_ID"))

    alert_jobs, job_executions = update_statistics(job_executions)

    send_notification(alert_jobs)

    # Upload document
    with open("job_executions.json", "w") as f:
        json.dump(job_executions, f)
    ctx.run(f"{AWS_S3_CP_CMD} {JOB_FAILURES_FILE} {S3_CI_BUCKET_URL}/{JOB_FAILURES_FILE} ", hide="stdout")


def retrieve_job_executions(ctx):
    """
    Retrieve the stored document in aws s3, or create it
    """
    try:
        ctx.run(f"{AWS_S3_CP_CMD}  {S3_CI_BUCKET_URL}/{JOB_FAILURES_FILE} {JOB_FAILURES_FILE}", hide=True)
        with open(JOB_FAILURES_FILE) as f:
            job_executions = json.load(f)
    except UnexpectedExit as e:
        if "404" in e.result.stderr:
            job_executions = create_initial_job_executions()
        else:
            raise e
    return job_executions


def create_initial_job_executions():
    job_executions = {"pipeline_id": 0, "jobs": {}}
    with open(JOB_FAILURES_FILE, "w") as f:
        json.dump(job_executions, f)
    return job_executions


def update_statistics(job_executions):
    # Update statistics and collect consecutive failed jobs
    alert_jobs = {"consecutive": [], "cumulative": []}
    failed_jobs = get_failed_jobs(PROJECT_NAME, os.getenv("CI_PIPELINE_ID"))
    failed_set = {job["name"] for job in failed_jobs.all_failures()}
    current_set = set(job_executions["jobs"].keys())
    # Insert data for newly failing jobs
    new_failed_jobs = failed_set - current_set
    for job in new_failed_jobs:
        job_executions["jobs"][job] = {"consecutive_failures": 1, "cumulative_failures": [1]}
    # Reset information for no-more failing jobs
    solved_jobs = current_set - failed_set
    for job in solved_jobs:
        job_executions["jobs"][job]["consecutive_failures"] = 0
        job_executions["jobs"][job]["cumulative_failures"].append(0)
        # Truncate the cumulative failures list
        if len(job_executions["jobs"][job]["cumulative_failures"]) > CUMULATIVE_LENGTH:
            job_executions["jobs"][job]["cumulative_failures"].pop(0)
    # Update information for still failing jobs and save them if they hit the threshold
    consecutive_failed_jobs = failed_set & current_set
    for job in consecutive_failed_jobs:
        job_executions["jobs"][job]["consecutive_failures"] += 1
        job_executions["jobs"][job]["cumulative_failures"].append(1)
        # Truncate the cumulative failures list
        if len(job_executions["jobs"][job]["cumulative_failures"]) > CUMULATIVE_LENGTH:
            job_executions["jobs"][job]["cumulative_failures"].pop(0)
        # Save the failed job if it hits the threshold
        if job_executions["jobs"][job]["consecutive_failures"] == CONSECUTIVE_THRESHOLD:
            alert_jobs["consecutive"].append(job)
        if sum(job_executions["jobs"][job]["cumulative_failures"]) == CUMULATIVE_THRESHOLD:
            alert_jobs["cumulative"].append(job)
    return alert_jobs, job_executions


def send_notification(alert_jobs):
    message = ""
    if len(alert_jobs["consecutive"]) > 0:
        jobs = ", ".join(f"`{j}`" for j in alert_jobs["consecutive"])
        message += f"Job(s) {jobs} failed {CONSECUTIVE_THRESHOLD} times in a row.\n"
    if len(alert_jobs["cumulative"]) > 0:
        jobs = ", ".join(f"`{j}`" for j in alert_jobs["cumulative"])
        message += f"Job(s) {jobs} failed {CUMULATIVE_THRESHOLD} times in last {CUMULATIVE_LENGTH} executions.\n"
    if message:
        send_slack_message("#agent-platform-ops", message)
