import glob
import io
import json
import os
import platform
import re
import subprocess
import tarfile
import tempfile
import xml.etree.ElementTree as ET

from invoke.exceptions import Exit

from tasks.flavor import AgentFlavor
from tasks.libs.pipeline_notifications import (
    DEFAULT_JIRA_PROJECT,
    DEFAULT_SLACK_CHANNEL,
    GITHUB_JIRA_MAP,
    GITHUB_SLACK_MAP,
)

CODEOWNERS_ORG_PREFIX = "@DataDog/"
REPO_NAME_PREFIX = "github.com/DataDog/datadog-agent/"
DATADOG_CI_COMMAND = ["datadog-ci", "junit", "upload"]
JOB_URL_FILE_NAME = "job_url.txt"
JOB_ENV_FILE_NAME = "job_env.txt"
TAGS_FILE_NAME = "tags.txt"


def add_flavor_to_junitxml(xml_path: str, flavor: AgentFlavor):
    """
    Takes a JUnit XML file and adds a flavor field to it, to allow tagging
    tests by flavor.
    """
    tree = ET.parse(xml_path)

    # Create a new element containing the flavor and append it to the tree
    flavor_element = ET.Element('flavor')
    flavor_element.text = flavor.name
    tree.getroot().append(flavor_element)

    # Write back to the original file
    tree.write(xml_path)


def junit_upload_from_tgz(junit_tgz, codeowners_path=".github/CODEOWNERS"):
    """
    Upload all JUnit XML files contained in given tgz archive.
    """
    from codeowners import CodeOwners

    with open(codeowners_path) as f:
        codeowners = CodeOwners(f.read())

    flaky_tests = get_flaky_from_test_output()

    # handle weird kitchen bug where it places the tarball in a subdirectory of the same name
    if os.path.isdir(junit_tgz):
        tmp_tgz = os.path.join(junit_tgz, os.path.basename(junit_tgz))
        if not os.path.isfile(tmp_tgz):
            tmp_tgz = os.path.join(junit_tgz, "junit.tar.gz")
        junit_tgz = tmp_tgz

    xmlcounts = {}
    with tempfile.TemporaryDirectory() as unpack_dir:
        # unpack all files from archive
        with tarfile.open(junit_tgz) as tgz:
            tgz.extractall(path=unpack_dir)
        # read additional tags
        tags = None
        tagsfile = os.path.join(unpack_dir, TAGS_FILE_NAME)
        if os.path.exists(tagsfile):
            with open(tagsfile) as tf:
                tags = tf.read().split()
        process_env = _update_environ(unpack_dir)
        # for each unpacked xml file, split it and submit all parts
        # NOTE: recursive=True is necessary for "**" to unpack into 0-n dirs, not just 1
        xmls = 0
        for xmlfile in glob.glob(f"{unpack_dir}/**/*.xml", recursive=True):
            if not os.path.isfile(xmlfile):
                print(f"[WARN] Matched folder named {xmlfile}")
                continue
            xmls += 1
            with tempfile.TemporaryDirectory() as output_dir:
                written_owners, flavor = split_junitxml(xmlfile, codeowners, output_dir, flaky_tests)
                upload_junitxmls(output_dir, written_owners, flavor, xmlfile.split("/")[-1], process_env, tags)
        xmlcounts[junit_tgz] = xmls

    empty_tgzs = []
    for tgz, count in xmlcounts.items():
        print(f"Submitted results for {count} JUnit XML files from {tgz}")
        if (
            count == 0 and "-fast" not in tgz
        ):  # *-fast(-v2).tgz contains only tests related to the modified code, they can be empty
            empty_tgzs.append(tgz)

    if empty_tgzs:
        raise Exit(f"[ERROR] No JUnit XML files for upload found in: {', '.join(empty_tgzs)}")


def get_flaky_from_test_output():
    """
    Read the test output file generated by gotestsum which contains a list of json for each unit test.
    We catch all tests marked as flaky in source code: they contain a certain message in the output field.
    """
    TEST_OUTPUT_FILES = ["test_output.json", "test_output_fast.json"]
    FLAKE_MESSAGE = "flakytest: this is a known flaky test"
    test_output = []
    for test_file in TEST_OUTPUT_FILES:
        if os.path.isfile(test_file):
            with open(test_file) as f:
                for line in f.readlines():
                    test_output.append(json.loads(line))
            break
    flaky_tests = [
        "/".join([test["Package"], test["Test"]]) for test in test_output if FLAKE_MESSAGE in test.get("Output", "")
    ]
    print(f"[INFO] Found {len(flaky_tests)} flaky tests.")
    return flaky_tests


def split_junitxml(xml_path, codeowners, output_dir, flaky_tests):
    """
    Split a junit XML into several according to the suite name and the codeowners.
    Returns a list with the owners of the written files.
    """
    tree = ET.parse(xml_path)
    output_xmls = {}

    flem = tree.find("flavor")
    flavor = flem.text if flem else AgentFlavor.base.name

    for suite in tree.iter("testsuite"):
        path = suite.attrib["name"].replace(REPO_NAME_PREFIX, "", 1)

        # Dirs in CODEOWNERS might end with "/", but testsuite names in JUnit XML
        # don't, so for determining ownership we append "/" temporarily.
        owners = codeowners.of(path + "/")
        if not owners:
            filepath = next(tree.iter("testcase")).attrib.get("file", None)
            if filepath:
                owners = codeowners.of(filepath)
                main_owner = owners[0][1][len(CODEOWNERS_ORG_PREFIX) :]
            else:
                main_owner = "none"
        else:
            main_owner = owners[0][1][len(CODEOWNERS_ORG_PREFIX) :]

        try:
            xml = output_xmls[main_owner]
        except KeyError:
            xml = ET.ElementTree(ET.Element("testsuites"))
            output_xmls[main_owner] = xml
        # Flag the test as known flaky if gotestsum already knew it
        for test_case in suite.iter("testcase"):
            test_name = "/".join([test_case.attrib["classname"], test_case.attrib["name"]])
            test_case.attrib["agent_is_known_flaky"] = "true" if test_name in flaky_tests else "false"

        xml.getroot().append(suite)

    for owner, xml in output_xmls.items():
        filepath = os.path.join(output_dir, owner + ".xml")
        xml.write(filepath, encoding="UTF-8", xml_declaration=True)

    return list(output_xmls), flavor


def upload_junitxmls(output_dir, owners, flavor, xmlfile_name, process_env, additional_tags=None):
    """
    Upload all per-team split JUnit XMLs from given directory.
    """
    processes = []

    for owner in owners:
        junit_file_path = os.path.join(output_dir, owner + ".xml")
        codeowner = CODEOWNERS_ORG_PREFIX + owner
        slack_channel = GITHUB_SLACK_MAP.get(codeowner.lower(), DEFAULT_SLACK_CHANNEL)[1:]
        jira_project = GITHUB_JIRA_MAP.get(codeowner.lower(), DEFAULT_JIRA_PROJECT)[0:]
        args = [
            "--service",
            "datadog-agent",
            "--tags",
            f'test.codeowners:["{codeowner}"]',
            "--tags",
            f"test.flavor:{flavor}",
            "--tags",
            f"slack_channel:{slack_channel}",
            "--tags",
            f"jira_project:{jira_project}",
        ]
        if additional_tags and "upload_option.os_version_from_name" in additional_tags:
            additional_tags.remove("upload_option.os_version_from_name")
            additional_tags.append("--tags")
            version_match = re.search(r"kitchen-rspec-([a-zA-Z0-9]+)-?([0-9-]*)-.*\.xml", xmlfile_name)
            exact_version = version_match.group(1) + version_match.group(2).replace("-", ".")
            additional_tags.append(f"version:{exact_version}")
            print(additional_tags)

        if additional_tags:
            args.extend(additional_tags)
        args.append(junit_file_path)
        processes.append(subprocess.Popen(DATADOG_CI_COMMAND + args, bufsize=-1, env=process_env))
    for process in processes:
        exit_code = process.wait()
        if exit_code != 0:
            raise subprocess.CalledProcessError(exit_code, DATADOG_CI_COMMAND)


def _update_environ(unpack_dir):
    """
    Add job_url and job_env to current env if any, for the junit upload command
    """
    process_env = os.environ.copy()
    # read job url (see comment in produce_junit_tar)
    job_url = None
    urlfile = os.path.join(unpack_dir, JOB_URL_FILE_NAME)
    if os.path.exists(urlfile):
        with open(urlfile) as jf:
            job_url = jf.read()
        print(f"CI_JOB_URL={job_url}")
        process_env["CI_JOB_URL"] = job_url

    job_env = {}
    envfile = os.path.join(unpack_dir, JOB_ENV_FILE_NAME)
    if os.path.exists(envfile):
        with open(envfile) as jf:
            for line in jf:
                if not line.strip():
                    continue
                key, val = line.strip().split('=', 1)
                job_env[key] = val
        print("\n".join(f"{k}={v}" for k, v in job_env.items()))
        process_env.update(job_env)
    return process_env


def _normalize_architecture(architecture):
    architecture = architecture.lower()
    normalize_table = {"amd64": "x86_64"}
    return normalize_table.get(architecture, architecture)


def produce_junit_tar(files, result_path):
    """
    Produce a tgz file containing all given files JUnit XML files and add a special file
    with additional tags.
    """
    # NOTE: for now, we can't pass CI_JOB_URL through `--tags`, because
    # the parsing logic tags breaks on URLs, as they contain colons.
    # Therefore we pass it through environment variable.
    tags = {
        "os.platform": platform.system().lower(),
        "os.architecture": _normalize_architecture(platform.machine()),
        "ci.job.name": os.environ.get("CI_JOB_NAME", ""),
        # "ci.job.url": os.environ.get("CI_JOB_URL", ""),
    }
    with tarfile.open(result_path, "w:gz") as tgz:
        for f in files:
            tgz.add(f, arcname=f.replace(os.path.sep, "-"))

        tags_file = io.BytesIO()
        for k, v in tags.items():
            tags_file.write(f"--tags {k}:{v} ".encode("UTF-8"))
        tags_info = tarfile.TarInfo(TAGS_FILE_NAME)
        tags_info.size = tags_file.getbuffer().nbytes
        tags_file.seek(0)
        tgz.addfile(tags_info, tags_file)

        job_url_file = io.BytesIO()
        job_url_file.write(os.environ.get("CI_JOB_URL", "").encode("UTF-8"))
        job_url_info = tarfile.TarInfo(JOB_URL_FILE_NAME)
        job_url_info.size = job_url_file.getbuffer().nbytes
        job_url_file.seek(0)
        tgz.addfile(job_url_info, job_url_file)


def repack_macos_junit_tar(workflow_conclusion, infile, outfile):
    """
    Repacks JUnit tgz file from macOS Github Action run, so it would
    contain correct job name and job URL.
    """
    if workflow_conclusion == "cancelled" and not os.path.exists(infile):
        print(f"Skipping repacking of JUnit tarball due to {workflow_conclusion} workflow")
        return

    with tarfile.open(infile) as infp, tarfile.open(outfile, "w:gz") as outfp, tempfile.TemporaryDirectory() as tempd:
        infp.extractall(tempd)

        # write the proper job url and job name
        with open(os.path.join(tempd, JOB_URL_FILE_NAME), "w") as fp:
            fp.write(os.environ.get("CI_JOB_URL", ""))
        with open(os.path.join(tempd, TAGS_FILE_NAME)) as fp:
            tags = fp.read()
        job_name = os.environ.get("CI_JOB_NAME", "")
        tags = tags.replace("ci.job.name:", f"ci.job.name:{job_name}")
        with open(os.path.join(tempd, TAGS_FILE_NAME), "w") as fp:
            fp.write(tags)

        # pack all files to a new tarball
        for f in os.listdir(tempd):
            outfp.add(os.path.join(tempd, f), arcname=f)
