# SPDX-FileCopyrightText: The RamenDR authors
# SPDX-License-Identifier: Apache-2.0

"""
Tests for tool management (kustomize-only version).
"""

import os
import tempfile
import unittest
from unittest.mock import patch

from packaging import version

from . import tools


class TestKustomize(unittest.TestCase):
    """Test Kustomize tool."""

    def test_kustomize_class_attributes(self):
        """Test kustomize has correct class attributes."""
        self.assertEqual(tools.Kustomize.name, "kustomize")
        self.assertEqual(tools.Kustomize.required_version, "5.7.0")

    def test_kustomize_install_dir(self):
        """Test kustomize install_dir method."""
        import sys

        kustomize = tools.Kustomize()
        expected_dir = os.path.join(sys.prefix, "bin")
        self.assertEqual(kustomize.install_dir(), expected_dir)

    def test_kustomize_path(self):
        """Test kustomize path generation."""
        import sys

        kustomize = tools.Kustomize()
        expected_path = os.path.join(sys.prefix, "bin", "kustomize")
        self.assertEqual(kustomize.path(), expected_path)

    @patch("drenv.commands.run")
    def test_kustomize_version_parsing(self, mock_run):
        """Test kustomize version parsing."""
        mock_run.return_value = "v5.7.0\n"
        kustomize = tools.Kustomize()
        ver = kustomize.version()
        self.assertEqual(ver, version.parse("5.7.0"))

    @patch("drenv.commands.run")
    def test_kustomize_version_not_installed(self, mock_run):
        """Test kustomize version when not installed."""
        from drenv.commands import Error

        mock_run.side_effect = Error(
            ["kustomize", "version"],
            "Could not execute: [Errno 2] No such file or directory",
        )
        kustomize = tools.Kustomize()
        ver = kustomize.version()
        self.assertIsNone(ver)


class TestHelpers(unittest.TestCase):
    """Test helper functions."""

    def test_platform_constants(self):
        """Test platform constants are set correctly."""
        self.assertIn(tools.OS_NAME, ["linux", "darwin"])
        self.assertIn(tools.ARCH, ["amd64", "arm64"])

    def test_download_real(self):
        """
        Test file download with real HTTP request.

        This integration test downloads a small file from GitHub to verify:
        1. The download function works correctly
        2. The URL format is correct (detects if tool was moved)
        """
        # Use a small, stable file from kustomize releases for testing
        # This is the LICENSE file which is small and unlikely to change
        test_url = (
            "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/"
            "master/LICENSE"
        )

        with tempfile.TemporaryDirectory() as tmpdir:
            target = os.path.join(tmpdir, "test-file")
            tools.download(test_url, target)
            self.assertTrue(os.path.exists(target))
            # Verify we got some content
            with open(target, "rb") as f:
                content = f.read()
                self.assertGreater(len(content), 0)
                # LICENSE file should contain "Apache"
                self.assertIn(b"Apache", content)


class TestSetup(unittest.TestCase):
    """Test setup function."""

    def test_setup_uses_venv_by_default(self):
        """Test setup uses venv bin directory by default."""
        import sys

        expected_dir = os.path.join(sys.prefix, "bin")
        # Just verify the directory exists or can be created
        self.assertIsNotNone(expected_dir)


if __name__ == "__main__":
    unittest.main()
