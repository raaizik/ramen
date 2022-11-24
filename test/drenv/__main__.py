# SPDX-FileCopyrightText: The RamenDR authors
# SPDX-License-Identifier: Apache-2.0

import argparse
import concurrent.futures
import copy
import logging
import os
import shutil
import subprocess
import sys
import time

from collections import deque

import yaml

import drenv

CMD_PREFIX = "cmd_"


def main():
    commands = [n[len(CMD_PREFIX):] for n in globals()
                if n.startswith(CMD_PREFIX)]

    p = argparse.ArgumentParser(prog="drenv")
    p.add_argument(
        "-v", "--verbose",
        action="store_true",
        help="Be more verbose")
    p.add_argument(
        "command",
        choices=commands,
        help="Command to run")
    p.add_argument(
        "filename",
        help="Environment filename")
    args = p.parse_args()

    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)-7s %(message)s")

    env = read_env(args.filename)

    func = globals()[CMD_PREFIX + args.command]
    func(env)


def read_env(filename):
    with open(filename) as f:
        env = yaml.safe_load(f)
    validate_env(env)
    return env


def validate_env(env):
    if "name" not in env:
        raise ValueError("Missing name")

    if "profiles" not in env:
        raise ValueError("Missing profiles")

    env.setdefault("templates", [])
    env.setdefault("workers", [])

    for template in env["templates"]:
        validate_template(template)

    bind_templates(env)

    for profile in env["profiles"]:
        validate_profile(profile)

    for i, worker in enumerate(env["workers"]):
        validate_worker(worker, env, i)


def validate_template(template):
    if "name" not in template:
        raise ValueError("Missing template name")


def bind_templates(env):
    templates = {t["name"]: t for t in env["templates"]}

    for i, profile in enumerate(env["profiles"]):
        # Ensure that profile is bound once.
        name = profile.pop("template", None)
        if name is None:
            continue

        if name not in templates:
            raise ValueError(f"Unknown template: {name}")

        # Deep copy the template so profiles do not share anything.
        template = copy.deepcopy(templates[name])

        # Merge template and profile, overiding template keys.
        env["profiles"][i] = {**template, **profile}


def validate_profile(profile):
    if "name" not in profile:
        raise ValueError("Missing profile name")

    profile.setdefault("container_runtime", "containerd")
    profile.setdefault("extra_disks", 0)
    profile.setdefault("disk_size", "20g")
    profile.setdefault("nodes", 1)
    profile.setdefault("cni", "auto")
    profile.setdefault("cpus", 2)
    profile.setdefault("memory", "4g")
    profile.setdefault("network", "")
    profile.setdefault("scripts", [])
    profile.setdefault("addons", [])
    profile.setdefault("workers", [])

    for i, worker in enumerate(profile["workers"]):
        validate_worker(worker, profile, i)


def validate_worker(worker, env, index):
    worker.setdefault("name", f'{env["name"]}/{index}')
    worker.setdefault("scripts", [])

    for script in worker["scripts"]:
        validate_script(script, env, args=[env["name"]])


def validate_script(script, env, args=()):
    if "file" not in script:
        raise ValueError(f"Missing script 'file': {script}")

    args = script.setdefault("args", list(args))

    for i, arg in enumerate(args):
        arg = arg.replace("$name", env["name"])
        args[i] = arg


def cmd_start(env):
    start = time.monotonic()
    logging.info("[%s] Starting environment", env["name"])
    execute(start_cluster, env["profiles"])
    execute(run_worker, env["workers"])
    logging.info(
        "[%s] Environment started in %.2f seconds",
        env["name"], time.monotonic() - start,
    )


def cmd_stop(env):
    start = time.monotonic()
    logging.info("[%s] Stopping environment", env["name"])
    execute(stop_cluster, env["profiles"])
    logging.info(
        "[%s] Environment stopped in %.2f seconds",
        env["name"], time.monotonic() - start,
    )


def cmd_delete(env):
    start = time.monotonic()
    logging.info("[%s] Deleting environment", env["name"])
    execute(delete_cluster, env["profiles"])
    logging.info(
        "[%s] Environment deleted in %.2f seconds",
        env["name"], time.monotonic() - start,
    )


def execute(func, profiles, delay=0.5):
    failed = False

    with concurrent.futures.ThreadPoolExecutor() as e:
        futures = {}

        for p in profiles:
            futures[e.submit(func, p)] = p["name"]
            time.sleep(delay)

        for f in concurrent.futures.as_completed(futures):
            try:
                f.result()
            except Exception:
                logging.exception("[%s] Cluster failed", futures[f])
                failed = True

    if failed:
        sys.exit(1)


def start_cluster(profile):
    start = time.monotonic()
    logging.info("[%s] Starting cluster", profile["name"])

    is_restart = drenv.cluster_info(profile["name"]) != {}

    minikube("start",
             "--driver", "kvm2",
             "--container-runtime", profile["container_runtime"],
             "--extra-disks", str(profile["extra_disks"]),
             "--disk-size", profile["disk_size"],
             "--network", profile["network"],
             "--nodes", str(profile["nodes"]),
             "--cni", profile["cni"],
             "--cpus", str(profile["cpus"]),
             "--memory", profile["memory"],
             "--addons", ",".join(profile["addons"]),
             profile=profile["name"])

    logging.info("[%s] Cluster started in %.2f seconds",
                 profile["name"], time.monotonic() - start)

    if is_restart:
        wait_for_deployments(profile)

    execute(run_worker, profile["workers"])


def stop_cluster(profile):
    start = time.monotonic()
    logging.info("[%s] Stopping cluster", profile["name"])
    minikube("stop", profile=profile["name"])
    logging.info("[%s] Cluster stopped in %.2f seconds",
                 profile["name"], time.monotonic() - start)


def delete_cluster(profile):
    start = time.monotonic()
    logging.info("[%s] Deleting cluster", profile["name"])
    minikube("delete", profile=profile["name"])
    profile_config = drenv.config_dir(profile["name"])
    if os.path.exists(profile_config):
        logging.info("[%s] Removing config %s",
                     profile["name"], profile_config)
        shutil.rmtree(profile_config)
    logging.info("[%s] Cluster deleted in %.2f seconds",
                 profile["name"], time.monotonic() - start)


def wait_for_deployments(profile, initial_wait=30, timeout=300):
    """
    When restarting, kubectl can report stale status for a while, before it
    starts to report real status. Then it takes a while until all deployments
    become available.

    We first sleep for initial_wait seconds, to give Kubernetes chance to fail
    liveness and readiness checks, and then wait until all deployments are
    available or the timeout has expired.

    TODO: Check if there is more reliable way to wait for actual status.
    """
    start = time.monotonic()
    logging.info(
        "[%s] Waiting until all deployments are available",
        profile["name"],
    )

    time.sleep(initial_wait)

    kubectl(
        "wait", "deploy", "--all",
        "--for", "condition=available",
        "--all-namespaces",
        "--timeout", f"{timeout}s",
        profile=profile["name"],
    )

    logging.info(
        "[%s] Deployments are available in %.2f seconds",
        profile["name"], time.monotonic() - start,
    )


def kubectl(cmd, *args, profile=None):
    minikube("kubectl", "--", cmd, *args, profile=profile)


def minikube(cmd, *args, profile=None):
    run("minikube", cmd, "--profile", profile, *args, name=profile)


def run_worker(worker):
    for script in worker["scripts"]:
        run_script(script, worker["name"])


def run_script(script, name):
    start = time.monotonic()
    logging.info("[%s] Starting %s", name, script["file"])
    run(script["file"], *script["args"], name=name)
    logging.info("[%s] %s completed in %.2f seconds",
                 name, script["file"], time.monotonic() - start)


def run(*cmd, name=None):
    # Avoid delays in child process logs.
    env = dict(os.environ)
    env["PYTHONUNBUFFERED"] = "1"

    p = subprocess.Popen(
        cmd,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        env=env,
    )

    messages = deque(maxlen=20)

    for line in iter(p.stdout.readline, b""):
        msg = line.decode().rstrip()
        messages.append(msg)
        logging.debug("[%s] %s", name, msg)

    p.wait()
    if p.returncode != 0:
        last_messages = "\n".join("  " + m for m in messages)
        raise RuntimeError(
            f"[{name}] Command {cmd} failed rc={p.returncode}\n"
            "\n"
            "Last messages:\n"
            f"{last_messages}")


if __name__ == "__main__":
    main()