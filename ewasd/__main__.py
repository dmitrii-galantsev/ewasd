"""Make ewasd package executable with python -m ewasd."""

from .cli import main

if __name__ == "__main__":
    raise SystemExit(main())
