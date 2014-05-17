package blogArticles

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	_ "github.com/mattn/go-sqlite3"
	"github.com/russross/blackfriday"
	"github.com/tgascoigne/akismet"
	"github.com/yumaikas/blogserv/config"
	die "github.com/yumaikas/golang-die"
)

type Article struct {
	Title, URL, Content, PublishStage string
	//This content doesn't come from my typing, it shouldn't be trusted.
	Comments       []Comment
	Next, Previous *Article
	IsAdmin        bool
}

func (art *Article) HTMLContent() string {
	output := blackfriday.MarkdownCommon([]byte(art.Content))
	return string(output)
}

var (
	ErrArticleNotFound error = errors.New("Article not found")
)

const (
	Published string = "Published"
	Draft     string = "Draft"
	Deleted   string = "Deleted"
)

//Handy for debugging things
func dump(me string) string {
	fmt.Println(me)
	return me
}

func IsDraft(ar Article) bool {
	return ar.PublishStage == Draft
}
func IsPublished(ar Article) bool {
	return ar.PublishStage == Published
}

//Hand ownership of the database handle to the calling method
func dbOpen() (*sql.DB, error) {
	db, err := sql.Open("sqlite3", config.DbPath())
	return db, err
}

func ListArticles() (arts []Article, retErr error) {
	fmt.Print("Listing Articles\n")
	defer func() {
		err := die.Log(recover())
		if err != nil {
			fmt.Println("An error occured while trying to fetch articles")
			arts = nil
			retErr = err.(error)
		}
	}()
	var ars = make([]Article, 0)

	db, err := dbOpen()
	defer db.Close()
	die.OnErr(err)

	//The article query
	rows, err := db.Query(`
	Select Title, URL, Content, PublishStage
	from Articles Order by id Desc`)
	die.OnErr(err)

	for rows.Next() {
		var ar Article
		rows.Scan(&ar.Title, &ar.URL, &ar.Content, &ar.PublishStage)
		ars = append(ars, ar)
	}

	die.OnErr(err)
	return ars, nil
}

func SaveArticle(ar Article) (retErr error) {
	defer func() {
		err := die.Log(recover())
		if err != nil {
			fmt.Println("An error occured while trying to save an article")
			retErr = err.(error)
		}
	}()
	db, err := dbOpen()
	defer db.Close()
	die.OnErr(err)

	var checkNum int
	db.QueryRow("Select Count(URL) from Articles where URL = ?", ar.URL).Scan(&checkNum)
	switch checkNum {
	case 0:
		//create article
		insert(ar)
	case 1:
		//update article
		update(ar)
	default:
		die.OnErr(errors.New("More than on article for a URL. Database integrity is compromised"))
	}

	return
}

func update(ar Article) {
	db, err := dbOpen()
	tx, err := db.Begin()
	defer func() {
		if val := die.Log(recover()); val != nil {
			fmt.Println("Article update failed!")
			tx.Rollback()
		}
	}()
	defer db.Close()

	fmt.Println("Attempting DB Open")
	die.OnErr(err)

	res, err := tx.Exec(`
	Update Articles
	set Title = ?, Content = ?, PublishStage = ?
	where URL = ?
	`, ar.Title, ar.Content, ar.PublishStage, ar.URL)
	cnt, err1 := res.RowsAffected()
	if cnt > 1 || err1 != nil || err != nil {
		tx.Rollback()
		panic(fmt.Sprintf("Update for %s Failed. %v rows would have been affected", ar.URL, cnt))
	}
	tx.Commit()
}

func insert(ar Article) {
	db, err := dbOpen()
	tx, err := db.Begin()
	if err != nil {
		panic("DB open failed")
	}
	defer db.Close()
	fmt.Println(ar.Title)
	fmt.Println(ar.URL)
	res, err := tx.Exec(`
	Insert into Articles(Title, Content, URL, PublishStage) 
	values (?, ?, ?, ?)
	`, ar.Title, ar.Content, ar.URL, ar.PublishStage)
	if err != nil {
		panic(err)
	}
	cnt, err1 := res.RowsAffected()
	if cnt > 1 || err1 != nil || err != nil {
		tx.Rollback()
		panic(fmt.Sprintf("Insert for %s Failed. %v rows would have been affected", ar.URL, cnt))
	}
	tx.Commit()
}

//Populates an article based on a title.
func FillArticle(URL string) (Article, error) {
	fmt.Println("Url searching", URL)
	var ar Article
	db, err := dbOpen()
	defer db.Close()
	if err != nil {
		return ar, err
	}

	var articleId int
	err = db.QueryRow(`
		Select Title, URL, Content, id, PublishStage
		from Articles 
		where URL = ?`,
		URL).Scan(&ar.Title, &ar.URL, &ar.Content, &articleId, &ar.PublishStage)
	if err != nil {
		fmt.Println("Testing for article search errors")
		switch err {
		case sql.ErrNoRows:
			return ar, ErrArticleNotFound
		default:
			//debug, for production use fmt.PrintF(err)
			log.Fatal(err)
			return ar, err
		}
	}
	getRow := func(id int) *sql.Row {
		return db.QueryRow(`Select Title, URL from Articles where id = ? `, id)
	}
	var n Article
	err = getRow(articleId+1).Scan(&n.Title, &n.URL)
	switch err {
	case nil:
		_ = ""
	case sql.ErrNoRows:
		_ = ""
	default:
		return ar, err
	}

	fmt.Println(n, "HEX")
	var p Article
	err = getRow(articleId-1).Scan(&p.Title, &p.URL)
	fmt.Println(err)
	switch err {
	case nil:
		_ = ""
	case sql.ErrNoRows:
		_ = ""
	default:
		return ar, err
	}
	fmt.Println(p, "CODE")
	ar.Next = &n
	ar.Previous = &p

	//Get the comments for the article
	commentQ, err := db.Prepare(`
	Select U.screenName, C.Content, C.Status, C.Guid from 
	Comments as C
	inner join Users as U on C.UserID = U.id
	where C.ArticleID = ?`)
	if err != nil {
		//debug, for production use fmt.PrintF(err)
		log.Fatal(err)
		return ar, err
	}

	rows, err := commentQ.Query(articleId)
	if err != nil {
		//debug, for production use fmt.PrintF(err)
		log.Fatal(err)
		return ar, err
	}

	ar.Comments = make([]Comment, 0)

	for rows.Next() {
		var c Comment
		err = rows.Scan(&c.UserName, &c.Content, &c.Status, &c.GUID)
		if err != nil {
			//debug, for production use fmt.Printf(err)
			log.Fatal(err)
			return ar, err
		}

		if len(c.Content) > 0 {
			ar.Comments = append(ar.Comments, c)
		}
	}
	if len(ar.Comments) == 0 {
		ar.Comments = nil
	}
	return ar, nil
}

//Add code to check for the user, and insert the user if need be
//These are the values that are populated in the comment.
/*
	    UserIP:      r.RemoteAddr,
		UserAgent:   r.UserAgent(),
		Author:      r.FormValue("author"),
		AuthorEmail: r.FormValue("email"),
		Content:     r.FormValue("Comment"),
*/

type queryComment struct {
	Sql  string
	Args func() (int, int, string)
}

//Currently do nothing
func SpamToDB(c akismet.Comment, arName string) error {
	return nil
}

func addUser(c akismet.Comment, tx *sql.Tx) (int, error) {
	//fmt.Print("Enter addUser")
	//defer fmt.Print("Exit addUser")
	r, err := tx.Exec("Insert into Users (screenName, Email) values (?, ?)",
		c.Author, c.AuthorEmail)
	if err != nil {
		return 0, err
	}
	cnt, err := r.RowsAffected()
	switch {
	case err != nil:
		return 0, err
	case cnt != 1:
		return 0, fmt.Errorf("%d rows affected instead of 1", cnt)
	}
	//Return the new userID
	if id, err := r.LastInsertId(); err == nil {
		return int(id), nil
	}
	return 0, err
}
