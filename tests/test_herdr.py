import unittest

from panopticon.herdr import (
    extract_error_code,
    extract_id,
    extract_path,
    extract_status,
    parse_json_output,
)


class HerdrParsingTests(unittest.TestCase):
    def test_extracts_nested_resource_ids_without_cli_wrapper_id(self) -> None:
        payload = {
            "id": "cli:request-1",
            "result": {
                "workspace": {"id": "w-workspace"},
                "tabId": "w-tab",
                "root_pane": {"pane_id": "w-pane"},
            },
        }

        self.assertEqual(extract_id(payload, "workspace"), "w-workspace")
        self.assertEqual(extract_id(payload, "tab"), "w-tab")
        self.assertEqual(extract_id(payload, "pane"), "w-pane")

    def test_extracts_path_status_and_error_code_from_nested_payload(self) -> None:
        payload = {
            "result": {
                "worktree": {"checkout_path": "/tmp/worktree"},
                "agent": {"state": "BLOCKED"},
                "error": {"code": "agent blocked"},
            }
        }

        self.assertEqual(extract_path(payload), "/tmp/worktree")
        self.assertEqual(extract_status(payload), "blocked")
        self.assertEqual(extract_error_code(payload), "agent_blocked")

        get_payload = {
            "status": "ok",
            "result": {
                "agent": {"agent_status": " WORKING "},
                "type": "agent_info",
            },
        }
        self.assertEqual(extract_status(get_payload), "working")

    def test_parse_json_output_ignores_noisy_lines_and_uses_last_document(self) -> None:
        output = 'log before\n{"status": "old"}\nlog after\n{"status": "new"}\n'

        self.assertEqual(parse_json_output(output), {"status": "new"})


if __name__ == "__main__":
    unittest.main()
