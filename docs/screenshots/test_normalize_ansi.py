"""Exercise the ANSI conversion used for Freeze captures, through its CLI."""

import pathlib
import subprocess
import sys
import unittest


class NormalizeAnsiTest(unittest.TestCase):
    def normalize(self, text):
        result = subprocess.run(
            [sys.executable, str(pathlib.Path(__file__).with_name("normalize-ansi.py"))],
            input=text,
            text=True,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout

    def test_default_foreground_preserves_row_background(self):
        self.assertEqual(
            self.normalize("\x1b[48;5;236mref \x1b[38;5;246mok\x1b[39m title  owner   "),
            "\x1b[0m\x1b[0m\x1b[48;5;236mref "
            "\x1b[0m\x1b[38;5;246;48;5;236mok"
            "\x1b[0m\x1b[48;5;236m title  owner   ",
        )

    def test_default_background_does_not_extend_a_zebra_band(self):
        self.assertEqual(
            self.normalize("\x1b[48;5;236mfirst   \n\x1b[49msecond  "),
            "\x1b[0m\x1b[0m\x1b[48;5;236mfirst   \n"
            "\x1b[0m\x1b[48;5;236m\x1b[0msecond  ",
        )

    def test_attribute_resets_leave_plain_titles_unstyled(self):
        self.assertEqual(
            self.normalize("\x1b[1;2;38;5;196;48;5;24mstuck\x1b[22;39m Title"),
            "\x1b[0m\x1b[0m\x1b[1;2;38;5;196;48;5;24mstuck"
            "\x1b[0m\x1b[48;5;24m Title",
        )

    def test_truecolor_and_full_reset(self):
        self.assertEqual(
            self.normalize("\x1b[38;2;1;2;3;48;2;4;5;6mtext\x1b[m plain"),
            "\x1b[0m\x1b[0m\x1b[38;2;1;2;3;48;2;4;5;6mtext\x1b[0m plain",
        )

    def test_preserves_unicode_and_full_width_padding(self):
        self.assertEqual(self.normalize("▶ ↵ — title     \n"), "\x1b[0m▶ ↵ — title     \n\x1b[0m")


if __name__ == "__main__":
    unittest.main()
