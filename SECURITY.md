# Security

If you find a vulnerability in the agent, please do not open a public issue. Write to the maintainers through the contact listed on https://github.com/ZeniFlow or open a private security advisory on GitHub (Security → Report a vulnerability).

The agent runs as root and executes commands from the panel it is enrolled to. Treat the panel's credentials and the node's `/etc/zeniflow-agent` directory (private key and client certificate) as secrets.
