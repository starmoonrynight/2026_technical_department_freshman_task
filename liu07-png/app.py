from flask import Flask, request, jsonify, session, send_file
import sqlite3
from datetime import datetime

app = Flask(__name__)
app.secret_key = 'hdu_2026_zhushou'

def get_db():
    conn = sqlite3.connect('lost_found.db')
    conn.row_factory = sqlite3.Row
    return conn

def init_db():
    conn = get_db()
    c = conn.cursor()
    c.execute('CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT UNIQUE, password TEXT)')
    c.execute('CREATE TABLE IF NOT EXISTS items (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER, type TEXT, title TEXT, desc TEXT, location TEXT, status TEXT, time TEXT)')
    conn.commit()

init_db()

@app.route('/')
def index():
    return send_file('index.html')

@app.route('/api/register', methods=['POST'])
def register():
    d = request.json
    conn = get_db()
    try:
        conn.execute("INSERT INTO users (username, password) VALUES (?, ?)", (d['username'], d['password']))
        conn.commit()
        return jsonify({'msg': '注册成功'})
    except:
        return jsonify({'msg': '用户名已存在'}), 400
    finally:
        conn.close()

@app.route('/api/login', methods=['POST'])
def login():
    d = request.json
    conn = get_db()
    u = conn.execute("SELECT * FROM users WHERE username=? AND password=?", (d['username'], d['password'])).fetchone()
    conn.close()
    if u:
        session['uid'] = u['id']
        session['uname'] = u['username']
        return jsonify({'msg': '登录成功', 'username': u['username']})
    return jsonify({'msg': '账号或密码错误'}), 401

@app.route('/api/me')
def me():
    if 'uid' not in session: return jsonify({'msg': '未登录'}), 401
    return jsonify({'id': session['uid'], 'username': session['uname']})

@app.route('/api/logout', methods=['POST'])
def logout():
    session.clear()
    return jsonify({'msg': '已退出'})

@app.route('/api/items', methods=['GET'])
def get_items():
    kw = request.args.get('keyword', '')
    sql = "SELECT items.*, users.username FROM items JOIN users ON items.user_id = users.id"
    params = []
    if kw:
        sql += " WHERE items.title LIKE ? OR items.desc LIKE ?"
        params = [f'%{kw}%', f'%{kw}%']
    sql += " ORDER BY items.time DESC"
    rows = get_db().execute(sql, params).fetchall()
    return jsonify([dict(r) for r in rows])

@app.route('/api/items', methods=['POST'])
def add_item():
    if 'uid' not in session: return jsonify({'msg': '请先登录'}), 401
    d = request.json
    conn = get_db()
    conn.execute("INSERT INTO items (user_id, type, title, desc, location, status, time) VALUES (?, ?, ?, ?, ?, ?, ?)",
                 (session['uid'], d['type'], d['title'], d.get('desc', ''), d.get('location', ''), '寻找中', datetime.now().strftime("%Y-%m-%d %H:%M")))
    conn.commit()
    conn.close()
    return jsonify({'msg': '发布成功'})

# 修改信息 + 状态管理都在这个接口里
@app.route('/api/items/<int:item_id>', methods=['PUT'])
def update_item(item_id):
    if 'uid' not in session: return jsonify({'msg': '请先登录'}), 401
    d = request.json
    conn = get_db()
    item = conn.execute("SELECT user_id FROM items WHERE id=?", (item_id,)).fetchone()
    
    if not item or item['user_id'] != session['uid']:
        conn.close()
        return jsonify({'msg': '无权操作'}), 403
        
    # 如果传了title，说明是修改信息；如果传了status，说明是状态管理
    if 'title' in d:
        conn.execute("UPDATE items SET title=?, desc=?, location=? WHERE id=?", (d['title'], d.get('desc', ''), d.get('location', ''), item_id))
    if 'status' in d:
        conn.execute("UPDATE items SET status=? WHERE id=?", (d['status'], item_id))
        
    conn.commit()
    conn.close()
    return jsonify({'msg': '更新成功'})

@app.route('/api/items/<int:item_id>', methods=['DELETE'])
def delete_item(item_id):
    if 'uid' not in session: return jsonify({'msg': '请先登录'}), 401
    conn = get_db()
    item = conn.execute("SELECT user_id FROM items WHERE id=?", (item_id,)).fetchone()
    if not item or item['user_id'] != session['uid']:
        conn.close()
        return jsonify({'msg': '无权删除'}), 403
    conn.execute("DELETE FROM items WHERE id=?", (item_id,))
    conn.commit()
    conn.close()
    return jsonify({'msg': '删除成功'})

if __name__ == '__main__':
    app.run(debug=True, port=5000)