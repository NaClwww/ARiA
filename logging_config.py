import logging
from pathlib import Path


LOG_PATH = Path("aria.log")


def setup_logging() -> None:
    root_logger = logging.getLogger()
    if root_logger.handlers:
        return

    formatter = logging.Formatter("%(asctime)s %(levelname)s %(name)s %(message)s")

    stream_handler = logging.StreamHandler()
    stream_handler.setFormatter(formatter)

    file_handler = logging.FileHandler(LOG_PATH, encoding="utf-8")
    file_handler.setFormatter(formatter)

    root_logger.setLevel(logging.INFO)
    root_logger.addHandler(stream_handler)
    root_logger.addHandler(file_handler)
