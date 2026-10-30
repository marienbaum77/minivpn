"""Run: python3 -m unittest discover -s internal/node -p 'watchdog_test.py'"""
import unittest

from watchdog import is_idle, since

MB = 1 << 20
IDLE, WINDOW = 1200, 300


class IsIdle(unittest.TestCase):
    def test_background_noise_is_idle(self):
        samples = [[t, 30_000] for t in range(60, 1260, 60)]  # 30 KB a minute
        self.assertTrue(is_idle(samples, 1200, IDLE, WINDOW, MB))

    def test_one_busy_window_keeps_node(self):
        samples = [[t, 30_000] for t in range(60, 1260, 60)]
        samples.append([700, 50 * MB])  # a few minutes of stream
        self.assertFalse(is_idle(samples, 1200, IDLE, WINDOW, MB))

    def test_old_traffic_is_forgotten(self):
        samples = [[100, 500 * MB], [1500, 10_000]]
        self.assertTrue(is_idle(samples, 1500, IDLE, WINDOW, MB))

    def test_spread_traffic_counts_per_window(self):
        # 300 KB a minute is 1.5 MB per window: someone is reading pages.
        samples = [[t, 300_000] for t in range(60, 1260, 60)]
        self.assertFalse(is_idle(samples, 1200, IDLE, WINDOW, MB))

    def test_missed_ticks_count_as_idle(self):
        self.assertTrue(is_idle([], 5000, IDLE, WINDOW, MB))



class Since(unittest.TestCase):
    def test_difference_of_totals(self):
        self.assertEqual(since(5000, 3000), 2000)

    def test_first_run_counts_everything(self):
        self.assertEqual(since(5000, None), 5000)

    def test_restart_counts_from_zero(self):
        self.assertEqual(since(700, 3000), 700)


if __name__ == "__main__":
    unittest.main()
