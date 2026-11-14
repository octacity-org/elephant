"""OmniShip pipeline: test on four native platforms, package tagged releases,
and publish them as one GitHub release.

Run `omniship generate` after editing this file and commit the regenerated
files with it.
"""

import shutil
import subprocess
from pathlib import Path

from omniship import Pipeline
from omniship.core.execution import Architecture, OperatingSystem
from omniship.plugins.github import (
    GitHubActions,
    GitHubActionStep,
    GitHubMatrix,
    GitHubPullRequest,
    GitHubPush,
    GitHubPushedTag,
    GitHubRelease,
    GitHubRunner,
    GitHubWorkflow,
    GitHubWorkflowDispatch,
)
from omniship.plugins.go import Go, GoToolchain
from omniship.plugins.zig import ZigToolchain

ZOVA_VERSION = "v1.3.0"
ZOVA_COMMIT = "a30581ef7070ebc335fed06bbc46ea94a8326c5f"
ZOVA_DIR = ".deps/zova"

# One entry per release target, built natively on its own runner.
TARGETS = {
    "linux-arm64": {
        "runner": GitHubRunner.UBUNTU_24_04_ARM,
        "goos": "linux",
        "goarch": "arm64",
        "zig_target": "aarch64-linux-gnu",
        "binary": "elephant",
        "test_timeout": "5m",
    },
    "linux-amd64": {
        "runner": GitHubRunner.UBUNTU_24_04,
        "goos": "linux",
        "goarch": "amd64",
        "zig_target": "x86_64-linux-gnu",
        "binary": "elephant",
        "test_timeout": "5m",
    },
    "windows-amd64": {
        "runner": GitHubRunner.WINDOWS_2022,
        "goos": "windows",
        "goarch": "amd64",
        "zig_target": "x86_64-windows-gnu",
        "binary": "elephant.exe",
        # internal/app alone takes 4-5 minutes on these runners.
        "test_timeout": "10m",
    },
    "macos-arm64": {
        "runner": GitHubRunner.MACOS_15,
        "goos": "darwin",
        "goarch": "arm64",
        "zig_target": "aarch64-macos.14.0",
        "binary": "elephant",
        "test_timeout": "5m",
    },
}

github = GitHubActions(
    default_runner=GitHubRunner.UBUNTU_24_04,
    # Branches are tested through pull_request; pushing them as well would
    # run the same commits twice.
    check=GitHubWorkflow(
        file="check.yml",
        name="CI",
        triggers=[
            GitHubPush(branches=["main"]),
            GitHubPullRequest(),
            GitHubWorkflowDispatch(),
        ],
        cancel_superseded=True,
    ),
    build=GitHubWorkflow(file="build.yml", name="Build"),
    ship=GitHubWorkflow(
        file="ship.yml",
        name="Release",
        triggers=[GitHubPush(tags=["v*"])],
        cancel_superseded=True,
    ),
)
pipeline = Pipeline(targets=[github])

# setup-python is not in OmniShip's bundled lock, so it is pinned here.
SETUP_PYTHON = GitHubActionStep(
    "actions/setup-python@ece7cb06caefa5fff74198d8649806c4678c61a1",
    "Set up Python",
    {"python-version": "3.13"},
)
NATIVE_REQUIREMENTS = [
    GoToolchain(version_file="go.mod", cache=True),
    ZigToolchain(version="0.17.0"),
    github.checkout(repository="ata-sesli/zova", ref=ZOVA_COMMIT, path=ZOVA_DIR),
]


def native_job(timeout_minutes, retention_days=None):
    """A job that runs once on each target's own runner."""
    return github.job(
        runners=[target["runner"] for target in TARGETS.values()],
        matrix=GitHubMatrix(
            include=[
                {"runner": target["runner"].value, "target": name}
                for name, target in TARGETS.items()
            ]
        ),
        fail_fast=False,
        timeout_minutes=timeout_minutes,
        env={"ELEPHANT_TARGET": github.matrix("target"), "CGO_ENABLED": "1"},
        before_steps=[SETUP_PYTHON],
        retention_days=retention_days,
    )


def run(ctx, *arguments, cwd=None):
    """Run a command with the task environment and stream its output."""
    executable = shutil.which(arguments[0], path=ctx.env.get("PATH")) or arguments[0]
    process = subprocess.Popen(
        [executable, *arguments[1:]],
        cwd=cwd or ctx.workspace,
        env=ctx.env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    output = []
    for line in process.stdout:
        output.append(line)
        ctx.log.output(line.rstrip("\r\n"))
    if process.wait() != 0:
        ctx.fail(f"{' '.join(arguments)} failed")
    return "".join(output).strip()


def python(ctx):
    path = ctx.env.get("PATH")
    return shutil.which("python", path=path) or shutil.which("python3", path=path)


def current_target(ctx):
    """The matrix target in CI; the host's own target in a local run."""
    name = ctx.env.get("ELEPHANT_TARGET")
    if not name:
        os_name = {
            OperatingSystem.LINUX: "linux",
            OperatingSystem.MACOS: "macos",
            OperatingSystem.WINDOWS: "windows",
        }.get(ctx.host.os)
        arch = {Architecture.X86_64: "amd64", Architecture.ARM64: "arm64"}.get(
            ctx.host.architecture
        )
        name = f"{os_name}-{arch}"
    if name not in TARGETS:
        ctx.fail(f"unsupported target: {name}")
    return name, TARGETS[name]


def release_tag(ctx):
    if ctx.env.get("GITHUB_REF_TYPE") == "tag":
        return ctx.env["GITHUB_REF_NAME"]
    return None


def prepare_native(ctx, target):
    """Check the toolchain and dependencies, then build the Zova C ABI and
    point CGO at it for every later command of this task."""
    if run(ctx, "go", "env", "GOOS") != target["goos"]:
        ctx.fail(f"GOOS is not {target['goos']}")
    if run(ctx, "go", "env", "GOARCH") != target["goarch"]:
        ctx.fail(f"GOARCH is not {target['goarch']}")
    zova = run(
        ctx, "go", "list", "-m", "-f", "{{.Version}}",
        "github.com/ata-sesli/zova/bindings/go",
    )
    if zova != ZOVA_VERSION:
        ctx.fail(f"Zova Go binding is {zova}, expected {ZOVA_VERSION}")
    Go(ctx).mod_download()
    run(ctx, "go", "mod", "verify")

    # Include Zig runtime for external CGO linkers.
    run(ctx, python(ctx), "scripts/prepare_zova.py", f"{ZOVA_DIR}/build.zig")
    zova_dir = ctx.workspace / ZOVA_DIR
    run(
        ctx, "zig", "build", "c-abi", "-Doptimize=ReleaseSafe",
        f"-Dtarget={target['zig_target']}", cwd=zova_dir,
    )
    if target["goos"] == "darwin":
        run(ctx, "sh", "scripts/repack-darwin-c-abi.sh", cwd=zova_dir)
    if target["goos"] == "windows":
        ctx.env["CC"] = "gcc"
        run(ctx, "gcc", "--version")
    # A forward-slash Windows path, as `pwd -W` prints it.
    root = zova_dir.as_posix()
    ctx.env["CGO_CFLAGS"] = f"-I{root}/include"
    ldflags = f"-L{root}/zig-out/lib"
    if target["goos"] == "windows":
        # Zova's Zig runtime calls NT APIs; MinGW does not link NTDLL by default.
        ldflags += " -lntdll"
    ctx.env["CGO_LDFLAGS"] = ldflags
    if target["goos"] == "darwin":
        ctx.env["MACOSX_DEPLOYMENT_TARGET"] = "14.0"


def build_executable(ctx, target, version):
    binary = f"bin/{target['binary']}"
    run(
        ctx, "go", "build", "-trimpath", "-ldflags",
        f"-s -w -X elephant/internal/transport/mcp.Version={version}",
        "-o", binary, "./cmd/elephant",
    )
    reported = run(ctx, str(ctx.workspace / binary), "--version")
    if reported != f"elephant {version}":
        ctx.fail(f"{binary} --version printed {reported!r}")
    return binary


# Python task: the Zova C ABI and the CGO flags pointing at it must exist in
# the same job as the Go commands. Steps cannot carry environment variables
# from one step to the next, and the paths are only known at run time.
#
# On a tag run the tested executable and its license notices are forwarded to
# Build, which packages them without rebuilding Zova or Elephant.
@pipeline.check
def check(stage):
    @stage.task(
        requires=NATIVE_REQUIREMENTS,
        execution=native_job(30, 7),
        forward_to_build=True,
    )
    def native_tests(ctx):
        name, target = current_target(ctx)
        tag = release_tag(ctx)
        prepare_native(ctx, target)
        if tag is None:
            sha = ctx.env.get("GITHUB_SHA") or run(ctx, "git", "rev-parse", "HEAD")
            version = f"dev-{sha[:12]}"
        else:
            version = run(ctx, python(ctx), "scripts/release.py", "version", tag)

        unformatted = run(ctx, "gofmt", "-l", "cmd", "internal")
        if unformatted:
            ctx.fail(f"Go files need formatting:\n{unformatted}")
        run(
            ctx, python(ctx), "-m", "unittest", "discover",
            "-s", "scripts", "-p", "test_*.py", "-v",
        )

        go = Go(ctx)
        # Includes real MCP stdio processes.
        go.test(verbose=True, timeout=target["test_timeout"])
        go.test(race=True, timeout=target["test_timeout"])
        go.vet()
        binary = build_executable(ctx, target, version)
        if tag is None:
            return

        notices = ctx.workspace / ".deps/notices"
        run(ctx, "go", "mod", "vendor", "-o", ".deps/notices")
        goroot = Path(run(ctx, "go", "env", "GOROOT"))
        shutil.copyfile(goroot / "LICENSE", notices / "Go-LICENSE")
        shutil.copyfile(ctx.workspace / ZOVA_DIR / "LICENSE", notices / "Zova-LICENSE")
        # Distinct names, so the single Build job receives all four targets.
        ctx.artifacts.add(binary, name=f"executable-{name}")
        ctx.artifacts.add(notices, name=f"notices-{name}")


# Python task: archive names come from the tag and the license files from the
# forwarded notices directory, whose path is only known at run time.
# Sha256Manifest has no {tag} placeholder and would not reject a missing or
# unexpected archive; release.py does both.
@pipeline.build
def build(stage):
    @stage.task(
        execution=github.job(
            timeout_minutes=10, before_steps=[SETUP_PYTHON], retention_days=7
        ),
    )
    def package(ctx):
        tag = release_tag(ctx)
        if tag is None:
            ctx.fail("release archives are built only for a pushed tag")
        for name in TARGETS:
            binary = ctx.artifacts.get(f"executable-{name}")
            notices = ctx.artifacts.get(f"notices-{name}")
            if binary is None or notices is None:
                ctx.fail(f"Check did not forward the {name} executable and notices")
            archive = run(
                ctx, python(ctx), "scripts/release.py", "package", tag, name,
                str(binary.path), "--notices", str(notices.path),
            )
            ctx.artifacts.add(archive)
        # Fails unless dist holds exactly the four expected archives.
        manifest = run(ctx, python(ctx), "scripts/release.py", "checksums", tag, "dist")
        ctx.artifacts.add(manifest)


@pipeline.ship
def ship(stage):
    stage.task(
        GitHubRelease(
            tag=GitHubPushedTag(),
            notes="auto",
            # release.py rejects build metadata, so "auto" marks exactly the
            # tags containing "-", as the old workflow did.
            prerelease="auto",
        ),
        execution=github.job(timeout_minutes=10),
    )
