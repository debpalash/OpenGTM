
import sys
import os
import sqlite3

# Add the project root to the python path so imports work
current_dir = os.path.dirname(os.path.abspath(__file__))
# apps/api -> apps -> root
project_root = os.path.abspath(os.path.join(current_dir, "../../"))
sys.path.append(project_root)

# Import bcrypt directly to avoid passlib incompatibility
import bcrypt

def get_password_hash(password):
    return bcrypt.hashpw(password.encode('utf-8'), bcrypt.gensalt()).decode('utf-8')

def reset_password():
    db_path = os.path.join(project_root, "data", "data.db")
    print(f"Connecting to database at: {db_path}")
    
    conn = sqlite3.connect(db_path)
    cursor = conn.cursor()
    
    try:
        # Check if user exists
        cursor.execute("SELECT username FROM users WHERE username = ?", ("admin",))
        user = cursor.fetchone()
        
        if not user:
            print("User 'admin' not found. Creating it...")
            new_password = "admin123"
            hashed_password = get_password_hash(new_password)
            cursor.execute(
                "INSERT INTO users (username, hashed_password, is_active, is_admin) VALUES (?, ?, ?, ?)",
                ("admin", hashed_password, True, True)
            )
            conn.commit()
            print("Admin user created with password: admin123")
            return

        print(f"Resetting password for user: admin")
        new_password = "admin123"
        hashed_password = get_password_hash(new_password)
        
        cursor.execute("UPDATE users SET hashed_password = ? WHERE username = ?", (hashed_password, "admin"))
        conn.commit()
        
        if cursor.rowcount > 0:
            print("Password updated successfully.")
        else:
            print("No changes made.")
            
    except Exception as e:
        print(f"Error: {e}")
    finally:
        conn.close()

if __name__ == "__main__":
    reset_password()
