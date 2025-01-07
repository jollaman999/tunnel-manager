#!/bin/bash

# Check root privileges
if [ "$EUID" -ne 0 ]; then
    echo "Error: Please run as root"
    exit 1
fi

# Check current limits
echo "=== Current Limits ==="
echo -e "\nCurrent root session's soft limit (ulimit -n):"
ulimit -n
echo -e "\nCurrent root session's hard limit (ulimit -Hn):"
ulimit -Hn

# Check if the current hard limit is greater than or equal to 65535
current_hard_limit=$(ulimit -Hn)

if [ -n "$current_hard_limit" ] && [ "$current_hard_limit" -ge 65535 ]; then
    echo "Current hard nofile limit ($current_hard_limit) is greater than or equal to 65535. No changes needed."
    exit 0
fi

# Check limits.conf file
LIMITS_FILE="/etc/security/limits.conf"
if [ ! -f "$LIMITS_FILE" ]; then
    echo "Error: $LIMITS_FILE not found"
    exit 1
fi

# Backup only if backup doesn't exist for today
timestamp=$(date +%Y%m%d)
backup_file="${LIMITS_FILE}.backup_${timestamp}"
if [ ! -f "$backup_file" ]; then
    echo -e "\nCreating backup: $backup_file"
    cp $LIMITS_FILE "$backup_file"
else
    echo -e "\nBackup already exists for today: $backup_file"
fi

# Remove existing nofile settings and add new ones
echo "Updating limits.conf settings..."
sed -i '/nofile/d' $LIMITS_FILE

echo "* soft nofile 65535" >> $LIMITS_FILE
echo "* hard nofile 65535" >> $LIMITS_FILE

echo "Updated $LIMITS_FILE successfully"

# Show new settings
echo -e "\n=== New Settings ==="
echo "Updated limits.conf settings:"
grep "nofile" $LIMITS_FILE

echo -e "\nNote: Please log out and log back in for the changes to take effect"
echo "You can verify the new limits with 'ulimit -n' command"